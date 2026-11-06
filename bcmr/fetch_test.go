package bcmr

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testServer returns a TLS server and a Fetcher that trusts it, with nothing
// refused, since the server is on this device.
func testServer(t *testing.T, handler http.HandlerFunc, gateways ...string) (*httptest.Server, *Fetcher) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	f := newFetcher(gateways, "", server.Client().Transport)
	return server, f
}

// TestFetchChecksHash ensures a file that doesn't match its published hash is
// refused, and that the next URI is tried.
func TestFetchChecksHash(t *testing.T) {
	good := []byte(`{"good":true}`)
	server, f := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			_, _ = w.Write([]byte(`{"tampered":true}`))
			return
		}
		if r.URL.Path == "/page" {
			_, _ = w.Write([]byte("<html>Not here</html>"))
			return
		}
		_, _ = w.Write(good)
	})
	hash := sha256.Sum256(good)

	data, err := f.Fetch(context.Background(), hash, []string{server.URL + "/bad", server.URL + "/good"})
	if err != nil || string(data) != string(good) {
		t.Fatalf("got %q, %v", data, err)
	}
	data, err = f.Fetch(context.Background(), hash, []string{server.URL + "/bad"})
	if !errors.Is(err, ErrRegistryChanged) || string(data) != `{"tampered":true}` {
		t.Fatalf("got %q, %v", data, err)
	}
	data, err = f.Fetch(context.Background(), hash, []string{server.URL + "/page", server.URL + "/bad"})
	if !errors.Is(err, ErrRegistryChanged) || string(data) != `{"tampered":true}` {
		t.Fatalf("a page got in the way: %q, %v", data, err)
	}
	if _, err = f.Fetch(context.Background(), hash, []string{server.URL + "/page"}); !errors.Is(err, ErrRegistryUnavailable) {
		t.Fatalf("got %v", err)
	}
	if _, err := f.Fetch(context.Background(), hash, nil); !errors.Is(err, ErrRegistryUnavailable) {
		t.Fatalf("no URIs: %v", err)
	}
}

// TestFetchLocations ensures URIs turn into what the CHIP says they mean.
func TestFetchLocations(t *testing.T) {
	f := newFetcher([]string{"https://gw1.example/", "https://gw2.example"}, "", nil)
	for uri, want := range map[string][]string{
		"example.com":                {"https://example.com" + wellKnownPath},
		"https://example.com":        {"https://example.com" + wellKnownPath},
		"https://example.com/":       {"https://example.com/"},
		"example.com/path/r.json":    {"https://example.com/path/r.json"},
		"https://example.com/r.json": {"https://example.com/r.json"},
		"ipfs://bafkrei1":            {"https://gw1.example/ipfs/bafkrei1", "https://gw2.example/ipfs/bafkrei1"},
		"ipfs://bafkrei1/r.json":     {"https://gw1.example/ipfs/bafkrei1/r.json", "https://gw2.example/ipfs/bafkrei1/r.json"},
		"ipfs://":                    nil,
		"closed.example/ipfs/bafkrei1": {"https://closed.example/ipfs/bafkrei1",
			"https://gw1.example/ipfs/bafkrei1", "https://gw2.example/ipfs/bafkrei1"},
		"https://closed.example/ipfs/": {"https://closed.example/ipfs/"},
		"bafy1.ipfs.closed.example/i.png": {"https://bafy1.ipfs.closed.example/i.png",
			"https://gw1.example/ipfs/bafy1/i.png", "https://gw2.example/ipfs/bafy1/i.png"},
		"http://example.com/r.json": nil,
		"ftp://example.com/r.json":  nil,
		"file:///etc/passwd":        nil,
		"":                          nil,
		"https://":                  nil,
		"exa\u202emple.com":         nil,
		"https://" + strings.Repeat("a", maxURILen): nil,
	} {
		got := f.locations(uri)
		if len(got) == 0 && len(want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %v, want %v", uri, got, want)
		}
	}
}

// TestFetchChangedOnIPFS ensures another file from a gateway, or from a URL with
// an ipfs path, is refused outright, as ipfs names its content.
func TestFetchChangedOnIPFS(t *testing.T) {
	server, f := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved" {
			http.Redirect(w, r, "/ipfs/bafkrei1", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`{"tampered":true}`))
	})
	f.Gateways = []string{server.URL}
	hash := sha256.Sum256([]byte("good"))
	for _, uri := range []string{"ipfs://bafkrei1", server.URL + "/ipfs/bafkrei1", server.URL + "/moved"} {
		data, err := f.Fetch(context.Background(), hash, []string{uri})
		if !errors.Is(err, ErrRegistryUnavailable) || data != nil {
			t.Fatalf("%s: got %q, %v", uri, data, err)
		}
	}
}

// TestFetchGatewayAfterClosedHost ensures a URL with an ipfs path falls back on
// the wallet's gateways, as nftstorage.link made necessary.
func TestFetchGatewayAfterClosedHost(t *testing.T) {
	good := []byte("registry")
	server, f := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/gw/") {
			_, _ = w.Write(good)
			return
		}
		http.Error(w, "closed", http.StatusTooManyRequests)
	})
	f.Gateways = []string{server.URL + "/gw"}
	data, err := f.Fetch(context.Background(), sha256.Sum256(good), []string{server.URL + "/ipfs/bafkrei1"})
	if err != nil || string(data) != "registry" {
		t.Fatalf("got %q, %v", data, err)
	}
}

// TestFetchGateways ensures ipfs URIs go through the gateways in order, and
// that a failed gateway gives way to the next.
func TestFetchGateways(t *testing.T) {
	good := []byte("registry")
	var paths []string
	server, f := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/down") {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		_, _ = w.Write(good)
	})
	f.Gateways = []string{server.URL + "/down", server.URL + "/up"}
	data, err := f.Fetch(context.Background(), sha256.Sum256(good), []string{"ipfs://bafkrei1"})
	if err != nil || string(data) != "registry" {
		t.Fatalf("got %q, %v", data, err)
	}
	want := []string{"/down/ipfs/bafkrei1", "/up/ipfs/bafkrei1"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("asked for %v", paths)
	}
}

// TestFetchSizeLimit ensures a file past the limit isn't read whole.
func TestFetchSizeLimit(t *testing.T) {
	big := make([]byte, maxRegistrySize+1)
	server, f := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(big)
	})
	_, err := f.Fetch(context.Background(), sha256.Sum256(big), []string{server.URL + "/r"})
	if err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("got %v", err)
	}
}

// TestFetchRedirects ensures a few redirects are followed and an endless chain
// isn't.
func TestFetchRedirects(t *testing.T) {
	good := []byte("registry")
	server, f := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/once":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/final":
			_, _ = w.Write(good)
		default:
			http.Redirect(w, r, r.URL.Path+"x", http.StatusFound)
		}
	})
	hash := sha256.Sum256(good)
	if _, err := f.Fetch(context.Background(), hash, []string{server.URL + "/once"}); err != nil {
		t.Fatal(err)
	}
	_, err := f.Fetch(context.Background(), hash, []string{server.URL + "/loop"})
	if !errors.Is(err, ErrRegistryUnavailable) {
		t.Fatalf("got %v", err)
	}
}

// TestFetchRedirectToHTTP ensures a redirect can't leave https.
func TestFetchRedirectToHTTP(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the plain server was reached")
	}))
	defer plain.Close()
	server, f := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/r", http.StatusFound)
	})
	_, err := f.Fetch(context.Background(), sha256.Sum256([]byte("x")), []string{server.URL + "/r"})
	if !errors.Is(err, ErrRegistryUnavailable) {
		t.Fatalf("got %v", err)
	}
}

// TestFetchCache ensures a file is read once, that a damaged kept file is read
// again, and that the kept file isn't trusted for another hash.
func TestFetchCache(t *testing.T) {
	good := []byte("registry")
	var hits atomic.Int32
	server, f := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(good)
	})
	f.CacheDir = filepath.Join(t.TempDir(), "registries")
	hash := sha256.Sum256(good)
	uris := []string{server.URL + "/r"}
	for range 3 {
		data, err := f.Fetch(context.Background(), hash, uris)
		if err != nil || string(data) != "registry" {
			t.Fatalf("got %q, %v", data, err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("%d downloads", hits.Load())
	}
	if err := os.WriteFile(f.cachePath(hash), []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Fetch(context.Background(), hash, uris); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Fatalf("%d downloads after damage", hits.Load())
	}
}

// TestFetchTrim ensures the cache stays within its size, dropping the oldest.
func TestFetchTrim(t *testing.T) {
	f := newFetcher(nil, t.TempDir(), nil)
	chunk := make([]byte, maxCacheSize/2+1)
	var hashes [3][32]byte
	for i := range hashes {
		chunk[0] = byte(i)
		hashes[i] = sha256.Sum256(chunk)
		f.keep(hashes[i], chunk)
		old := time.Now().Add(time.Duration(i-10) * time.Hour)
		_ = os.Chtimes(f.cachePath(hashes[i]), old, old)
	}
	if f.cached(hashes[0]) != nil || f.cached(hashes[1]) != nil && f.cached(hashes[2]) == nil {
		t.Fatal("the oldest wasn't dropped")
	}
	if f.cached(hashes[2]) == nil {
		t.Fatal("the newest was dropped")
	}
}

// TestFetchCancel ensures a cancelled fetch stops at once rather than trying
// the other URIs.
func TestFetchCancel(t *testing.T) {
	var hits atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	server, f := testServer(t, func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
		cancel()
	})
	_, err := f.Fetch(ctx, sha256.Sum256([]byte("x")), []string{server.URL + "/a", server.URL + "/b"})
	if !errors.Is(err, context.Canceled) || hits.Load() != 1 {
		t.Fatalf("got %v after %d requests", err, hits.Load())
	}
}

// TestPublicAddress ensures addresses on this device or its network are
// refused, as a URI from a stranger could otherwise reach them.
func TestPublicAddress(t *testing.T) {
	for host, want := range map[string]bool{
		"8.8.8.8":         true,
		"2606:4700::1111": true,
		"127.0.0.1":       false,
		"::1":             false,
		"10.1.2.3":        false,
		"172.16.0.1":      false,
		"192.168.1.1":     false,
		"169.254.169.254": false,
		"fe80::1":         false,
		"fc00::1":         false,
		"0.0.0.0":         false,
		"224.0.0.1":       false,
		"100.64.1.1":      false,
		"198.18.0.1":      false,
		"192.0.0.1":       false,
		"240.0.0.1":       false,
		"0.1.2.3":         false,
		"64:ff9b::a00:1":  false,
		"2002:a00:1::1":   false,
		"fec0::1":         false,
	} {
		if got := publicAddress(net.ParseIP(host)); got != want {
			t.Errorf("%s: got %v", host, got)
		}
	}
	err := refusePrivate("tcp4", "192.168.1.1:443", nil)
	if !errors.Is(err, errPrivateAddress) {
		t.Fatalf("got %v", err)
	}
	if err := refusePrivate("tcp4", "8.8.8.8:443", nil); err != nil {
		t.Fatal(err)
	}
}

// TestNewFetcherRefusesLocal ensures the real transport won't reach a server on
// this device.
func TestNewFetcherRefusesLocal(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the local server was reached")
	}))
	defer server.Close()
	_, err := NewFetcher(nil, "", nil).Fetch(context.Background(), [32]byte{}, []string{server.URL + "/r"})
	if !errors.Is(err, ErrRegistryUnavailable) || !strings.Contains(err.Error(), "private") {
		t.Fatalf("got %v", err)
	}
}
