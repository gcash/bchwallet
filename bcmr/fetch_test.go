package bcmr

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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
		// Each from a fresh start, as the gateway that serves the wrong file
		// rests after each.
		f.healthMu.Lock()
		f.out = make(map[string]gatewayOut)
		f.healthMu.Unlock()
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
	gateway, f := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(good)
	})
	// The publisher is a host of its own, as its 429 rests only it.
	closed := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "closed", http.StatusTooManyRequests)
	}))
	t.Cleanup(closed.Close)
	f.Gateways = []string{gateway.URL + "/gw"}
	data, err := f.Fetch(context.Background(), sha256.Sum256(good), []string{closed.URL + "/ipfs/bafkrei1"})
	if err != nil || string(data) != "registry" {
		t.Fatalf("got %q, %v", data, err)
	}
}

// TestFetchGateways ensures ipfs URIs go through the gateways in order, and
// that a failed gateway gives way to the next.
func TestFetchGateways(t *testing.T) {
	good := []byte("registry")
	var mu sync.Mutex
	var paths []string
	asked := func(r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, r.Host+r.URL.Path)
	}
	up, f := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		asked(r)
		_, _ = w.Write(good)
	})
	down := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked(r)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	t.Cleanup(down.Close)
	f.Gateways = []string{down.URL + "/down", up.URL + "/up"}
	data, err := f.Fetch(context.Background(), sha256.Sum256(good), []string{"ipfs://bafkrei1"})
	if err != nil || string(data) != "registry" {
		t.Fatalf("got %q, %v", data, err)
	}
	want := []string{hostOf(down.URL) + "/down/ipfs/bafkrei1", hostOf(up.URL) + "/up/ipfs/bafkrei1"}
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

// TestFetchTrimKeepsRead ensures a kept file that's read again goes after those
// read longer ago, however long ago it was stored.
func TestFetchTrimKeepsRead(t *testing.T) {
	f := newFetcher(nil, t.TempDir(), nil)
	chunk := make([]byte, maxCacheSize/3+1)
	var hashes [3][32]byte
	for i := range hashes {
		chunk[0] = byte(i)
		hashes[i] = sha256.Sum256(chunk)
	}
	for i := range 2 {
		chunk[0] = byte(i)
		f.keep(hashes[i], chunk)
		old := time.Now().Add(time.Duration(i-10) * time.Hour)
		_ = os.Chtimes(f.cachePath(hashes[i]), old, old)
	}
	if f.cached(hashes[0]) == nil {
		t.Fatal("the first wasn't kept")
	}
	chunk[0] = 2
	f.keep(hashes[2], chunk)
	if f.cached(hashes[0]) == nil || f.cached(hashes[1]) != nil || f.cached(hashes[2]) == nil {
		t.Fatal("the one read longest ago wasn't the one dropped")
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
	f := NewFetcher(nil, "", nil)
	if f.roundPause != 30*time.Second {
		t.Fatalf("rounds %v apart", f.roundPause)
	}
	// An address that's always refused gets no more rounds, which would take
	// a pause each.
	start := time.Now()
	_, err := f.Fetch(context.Background(), [32]byte{}, []string{server.URL + "/r"})
	if !errors.Is(err, ErrRegistryUnavailable) || !strings.Contains(err.Error(), "private") {
		t.Fatalf("got %v", err)
	}
	if took := time.Since(start); took >= f.roundPause {
		t.Fatalf("took %v", took)
	}
}

// TestFetchTriesAFewLocations ensures a publication with many URIs can't hold up
// the other tokens for long.
func TestFetchTriesAFewLocations(t *testing.T) {
	var hits atomic.Int32
	server, f := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	})
	uris := make([]string, 100)
	for i := range uris {
		uris[i] = fmt.Sprintf("%s/r%d", server.URL, i)
	}
	if _, err := f.Fetch(context.Background(), [32]byte{1}, uris); !errors.Is(err, ErrRegistryUnavailable) {
		t.Fatalf("got %v", err)
	}
	if hits.Load() != maxLocations*3 {
		t.Fatalf("%d tries", hits.Load())
	}
}

// TestRefuseLocal ensures a connection through a proxy can't name this device or
// its network.
func TestRefuseLocal(t *testing.T) {
	for address, want := range map[string]bool{"localhost:443": true, "127.0.0.1:443": true,
		"10.0.0.1:443": true, "[::1]:443": true, "localhost.:443": true, "a.localhost:443": true,
		"127.1:443": true, "2130706433:443": true, "0x7f.1:443": true,
		"10.0.0.1.:443": true, "127.0.0.1.:443": true,
		"[::127.0.0.1]:443": true, "[::ffff:0:10.0.0.1]:443": true,
		"example.com:443": false, "8.8.8.8:443": false, "8.8.8.8.:443": false} {
		if (RefuseLocal(address) != nil) != want {
			t.Errorf("%s", address)
		}
	}
}

// TestDownloadTooSlow ensures a host that drips a file loses the download, and
// that one that sends it doesn't.
func TestDownloadTooSlow(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)
	server, f := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fast" {
			_, _ = w.Write([]byte("registry"))
			return
		}
		flusher := w.(http.Flusher)
		if r.URL.Path == "/steady" {
			for range 70 {
				_, _ = w.Write(make([]byte, 1024))
				flusher.Flush()
				time.Sleep(100 * time.Millisecond)
			}
			return
		}
		for {
			_, _ = w.Write([]byte("x"))
			flusher.Flush()
			select {
			case <-stop:
				return
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	})
	if _, _, err := f.download(context.Background(), f.registries, server.URL+"/fast", 100); err != nil {
		t.Fatal(err)
	}
	old := rateWindow
	rateWindow = time.Second
	defer func() { rateWindow = old }()
	start := time.Now()
	if _, _, err := f.download(context.Background(), f.registries, server.URL+"/drip", 1000); err == nil {
		t.Fatal("no error")
	}

	// Left alone, the drip takes 100 seconds.
	if time.Since(start) > 60*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}

	// A host that keeps to the rate gets through windows, here three of them.
	rateWindow = 2 * time.Second
	data, _, err := f.download(context.Background(), f.registries, server.URL+"/steady", 1<<20)
	if err != nil || len(data) != 70*1024 {
		t.Fatalf("%d bytes, %v", len(data), err)
	}
}

// deadlineSpy is a transport that tells whether a request had a deadline.
type deadlineSpy struct {
	next      http.RoundTripper
	deadlines atomic.Int32
}

func (s *deadlineSpy) RoundTrip(req *http.Request) (*http.Response, error) {
	if _, ok := req.Context().Deadline(); ok {
		s.deadlines.Add(1)
	}
	return s.next.RoundTrip(req)
}

// TestDownloadsHaveNoDeadline ensures neither a registry nor an icon download has a
// limit on its whole time, as a slow link has to get there, and that the caller's
// own deadline still holds.
func TestDownloadsHaveNoDeadline(t *testing.T) {
	file := []byte(`{"registry":true}`)
	server, _ := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(file)
	})
	spy := &deadlineSpy{next: server.Client().Transport}
	f := newFetcher(nil, "", spy)
	if _, err := f.Fetch(context.Background(), sha256.Sum256(file), []string{server.URL + "/r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.FetchIcon(context.Background(), server.URL+"/icon.png"); err != nil {
		t.Fatal(err)
	}
	if n := spy.deadlines.Load(); n != 0 {
		t.Fatalf("%d downloads had a deadline", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if _, err := f.FetchIcon(ctx, server.URL+"/icon.png"); err != nil {
		t.Fatal(err)
	}
	if spy.deadlines.Load() != 1 {
		t.Fatal("the caller's deadline was lost")
	}
}

// TestRateCheck ensures a download ends after two slow windows in a row, and only
// then.
func TestRateCheck(t *testing.T) {
	check := rateCheck{least: 100}
	for i, step := range []struct {
		read int64
		want bool
	}{
		{100, false}, // Fast enough.
		{100, false}, // Slow, once.
		{200, false}, // Fast enough, so what's slow starts over.
		{299, false}, // Slow, once.
		{300, true},  // Slow again.
	} {
		if got := check.tooSlow(step.read); got != step.want {
			t.Fatalf("window %d: %v", i, got)
		}
	}
}

// TestDownloadsPerHost ensures only so many downloads at once from a host, for each
// kind apart, that the rest wait in line, and that a host's line goes once empty.
func TestDownloadsPerHost(t *testing.T) {
	arrived := make(chan struct{}, 32)
	release := make(chan struct{})
	server, f := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		arrived <- struct{}{}
		<-release
		_, _ = w.Write([]byte("x"))
	})
	// Before the server closes, which waits for its handlers, if this fails.
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	t.Cleanup(finish)

	var done sync.WaitGroup
	start := func(slots *hostSlots, n int) {
		for range n {
			done.Add(1)
			go func() {
				defer done.Done()
				_, _, _ = f.download(context.Background(), slots, server.URL+"/x", 100)
			}()
		}
	}
	take := func(n int) {
		t.Helper()
		for range n {
			select {
			case <-arrived:
			case <-time.After(30 * time.Second):
				t.Fatal("a download didn't start")
			}
		}
	}
	start(f.registries, maxPerHost+3)
	take(maxPerHost)
	start(f.icons, maxPerHost)
	take(maxPerHost)

	// The downloads over the limit are in line, not under way.
	waitInLine(t, f.registries, 3)
	if len(arrived) != 0 {
		t.Fatalf("%d downloads over the limit", len(arrived))
	}
	finish()
	done.Wait()
	if len(f.registries.busy) != 0 || len(f.icons.busy) != 0 {
		t.Fatalf("%d and %d hosts kept", len(f.registries.busy), len(f.icons.busy))
	}
}

// TestSlotsInOrder ensures the line is first come, first served, and that leaving
// it gives up a place without losing a slot.
func TestSlotsInOrder(t *testing.T) {
	s := newHostSlots(1)
	held, err := s.take(context.Background(), "h")
	if err != nil {
		t.Fatal(err)
	}
	order := make(chan int, 3)
	gone, leave := context.WithCancel(context.Background())
	var waiting sync.WaitGroup
	for i, ctx := range []context.Context{context.Background(), gone, context.Background()} {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			release, err := s.take(ctx, "h")
			if err != nil {
				return
			}
			order <- i
			release()
		}()
		// Each joins the line before the next.
		for {
			s.mu.Lock()
			n := len(s.waiting)
			s.mu.Unlock()
			if n == i+1 {
				break
			}
			time.Sleep(time.Millisecond)
		}
	}
	leave()
	held()
	waiting.Wait()
	close(order)
	var got []int
	for i := range order {
		got = append(got, i)
	}
	if !reflect.DeepEqual(got, []int{0, 2}) {
		t.Fatalf("%v", got)
	}
	if len(s.busy) != 0 || len(s.waiting) != 0 {
		t.Fatal("the host was kept")
	}
}

// TestIconsDontWaitForRegistries ensures an icon has its own place at a host whose
// registry downloads are all running.
func TestIconsDontWaitForRegistries(t *testing.T) {
	server, f := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("icon"))
	})
	for range maxPerHost {
		if _, err := f.registries.take(context.Background(), hostOf(server.URL)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	data, err := f.FetchIcon(ctx, server.URL+"/icon.png")
	if err != nil || string(data) != "icon" {
		t.Fatalf("%q, %v", data, err)
	}
}

// TestSlotNotLostLeavingLine ensures a slot that reaches someone just as they leave
// the line goes on to the next, many times over, as the two can come together.
func TestSlotNotLostLeavingLine(t *testing.T) {
	s := newHostSlots(1)
	for i := range 200 {
		held, err := s.take(context.Background(), "h")
		if err != nil {
			t.Fatal(err)
		}
		ctx, leave := context.WithCancel(context.Background())
		left := make(chan struct{})
		go func() {
			if release, err := s.take(ctx, "h"); err == nil {
				release()
			}
			close(left)
		}()
		for {
			s.mu.Lock()
			n := len(s.waiting)
			s.mu.Unlock()
			if n == 1 {
				break
			}
			time.Sleep(10 * time.Microsecond)
		}
		leave()
		held()
		<-left
		got, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		release, err := s.take(got, "h")
		cancel()
		if err != nil {
			t.Fatalf("round %d: the slot was lost", i)
		}
		release()
	}
}

// TestRoutingTakesFirstWithRoom ensures a file on IPFS comes from the first gateway in
// the list with room, and from the first only while it has room.
func TestRoutingTakesFirstWithRoom(t *testing.T) {
	first := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("first"))
	}))
	t.Cleanup(first.Close)
	second, f := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("second"))
	})
	f.Gateways = []string{first.URL, second.URL}
	data, err := f.FetchIcon(context.Background(), "ipfs://bafy1/icon.png")
	if err != nil || string(data) != "first" {
		t.Fatalf("%q, %v", data, err)
	}
	for range maxPerHost {
		if _, err := f.icons.take(context.Background(), hostOf(first.URL)); err != nil {
			t.Fatal(err)
		}
	}
	data, err = f.FetchIcon(context.Background(), "ipfs://bafy1/icon.png")
	if err != nil || string(data) != "second" {
		t.Fatalf("%q, %v", data, err)
	}
}

// TestFreedSlotGoesToWhoCanUseIt ensures a slot that comes free goes to the first in
// line that can use its host, and that one who can use any host takes whichever
// comes free first.
func TestFreedSlotGoesToWhoCanUseIt(t *testing.T) {
	s := newHostSlots(1)
	a, _ := s.take(context.Background(), "a")
	b, _ := s.take(context.Background(), "b")
	got := make(chan string, 2)
	wait := func(hosts []string) {
		go func() {
			host, release, err := s.takeAny(context.Background(), hosts)
			if err == nil {
				got <- host
				release()
			}
		}()
	}
	inLine := func(n int) {
		for {
			s.mu.Lock()
			l := len(s.waiting)
			s.mu.Unlock()
			if l == n {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	wait([]string{"a"})
	inLine(1)
	wait([]string{"a", "b"})
	inLine(2)

	// b is free first: the first in line can't use it, so the second takes it.
	b()
	if host := <-got; host != "b" {
		t.Fatalf("%s", host)
	}
	a()
	if host := <-got; host != "a" {
		t.Fatalf("%s", host)
	}
}

// gatewayPair is a Fetcher with two gateways, the first answering with status
// until it's changed, and a clock the test moves.
type gatewayPair struct {
	f      *Fetcher
	status atomic.Int32
	hits   atomic.Int32

	// retryAfter is the first's Retry-After header, if it isn't nil.
	retryAfter atomic.Pointer[string]
	first      string
	clock      time.Time
	clockMu    sync.Mutex
}

func newGatewayPair(t *testing.T) *gatewayPair {
	t.Helper()
	p := &gatewayPair{clock: time.Unix(1_800_000_000, 0)}
	p.status.Store(http.StatusOK)
	first := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		p.hits.Add(1)
		if after := p.retryAfter.Load(); after != nil {
			w.Header().Set("Retry-After", *after)
		}
		if code := int(p.status.Load()); code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		_, _ = w.Write([]byte("first"))
	}))
	t.Cleanup(first.Close)
	second, f := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("second"))
	})
	f.Gateways = []string{first.URL, second.URL}
	f.now = func() time.Time {
		p.clockMu.Lock()
		defer p.clockMu.Unlock()
		return p.clock
	}
	p.f, p.first = f, hostOf(first.URL)
	return p
}

func (p *gatewayPair) pass(d time.Duration) {
	p.clockMu.Lock()
	p.clock = p.clock.Add(d)
	p.clockMu.Unlock()
}

func (p *gatewayPair) icon(t *testing.T) string {
	t.Helper()
	data, err := p.f.FetchIcon(context.Background(), "ipfs://bafy1/icon.png")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestFailingGatewayRests ensures a gateway that fails is out of rotation for longer
// each time in a row, up to five minutes, comes back after, and that one success
// puts it back for good.
func TestFailingGatewayRests(t *testing.T) {
	p := newGatewayPair(t)
	p.status.Store(http.StatusBadGateway)
	for _, rest := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute,
		4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {

		// It fails, and the second gateway has the file.
		if got := p.icon(t); got != "second" {
			t.Fatalf("%s", got)
		}
		// Out until the rest is over, so the first asked is the second.
		p.pass(rest - time.Second)
		if inRotationLen(p.f, p.first, "x") != 1 {
			t.Fatalf("back before %v", rest)
		}
		p.pass(time.Second)
		if inRotationLen(p.f, p.first, "x") != 2 {
			t.Fatalf("not back after %v", rest)
		}
	}
	p.status.Store(http.StatusOK)
	if got := p.icon(t); got != "first" {
		t.Fatalf("%s", got)
	}
	if len(p.f.out) != 0 {
		t.Fatal("a success didn't put it back")
	}
}

// TestWhatIsAFailure ensures a 404, or a caller giving up, leaves a gateway in, and a
// file that doesn't match its hash takes it out.
func TestWhatIsAFailure(t *testing.T) {
	p := newGatewayPair(t)
	p.status.Store(http.StatusNotFound)
	if got := p.icon(t); got != "second" {
		t.Fatalf("%s", got)
	}
	if len(p.f.out) != 0 {
		t.Fatal("a 404 took the gateway out")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = p.f.FetchIcon(ctx, "ipfs://bafy1/icon.png")
	if len(p.f.out) != 0 {
		t.Fatal("giving up took the gateway out")
	}

	p.status.Store(http.StatusOK)
	raw := "ipfs://bafkreicqqq3tkq5tkbtg3vhbhw6urybothpoedsszuejs5baq2k25mhgvu"
	_, _ = p.f.FetchIcon(context.Background(), raw)
	if _, ok := p.f.out[p.first]; !ok {
		t.Fatal("a file that failed its hash left the gateway in")
	}
}

// inRotationLen returns how many of the hosts are in rotation.
func inRotationLen(f *Fetcher, hosts ...string) int {
	in, _ := f.inRotation(hosts)
	return len(in)
}

// TestAllGatewaysOut ensures that with every gateway out, an icon the user asked for
// is told so at once, while what loads by itself, and a registry the user asked
// for, waits for one to come back, and that a wait can be given up.
func TestAllGatewaysOut(t *testing.T) {
	file := []byte(`{"r":1}`)
	server, f := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ipfs/bafy1/icon.png" {
			_, _ = w.Write([]byte("icon"))
			return
		}
		_, _ = w.Write(file)
	})
	f.Gateways = []string{server.URL}
	host := hostOf(server.URL)
	rest := func(d time.Duration) {
		f.healthMu.Lock()
		f.out[host] = gatewayOut{failures: 1, until: time.Now().Add(d)}
		f.healthMu.Unlock()
	}

	rest(time.Hour)
	// Told at once, with no round after a pause.
	f.roundPause = time.Hour
	limit, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	if _, err := f.FetchIcon(UserAction(limit), "ipfs://bafy1/icon.png"); !errors.Is(err, ErrNoGateway) {
		t.Fatalf("the user wasn't told: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.FetchIcon(ctx, "ipfs://bafy1/icon.png"); !errors.Is(err, context.Canceled) {
		t.Fatalf("a wait given up: %v", err)
	}

	// Waits can only be longer than this, never shorter, on a slow machine.
	const back = 200 * time.Millisecond
	for _, test := range []struct {
		name  string
		fetch func() ([]byte, error)
		want  []byte
	}{
		{"an icon that loads by itself", func() ([]byte, error) {
			return f.FetchIcon(context.Background(), "ipfs://bafy1/icon.png")
		}, []byte("icon")},
		{"a registry the user asked for", func() ([]byte, error) {
			return f.Fetch(UserAction(context.Background()), sha256.Sum256(file), []string{"ipfs://bafy1/r.json"})
		}, file},
	} {
		rest(back)
		start := time.Now()
		data, err := test.fetch()
		if err != nil || string(data) != string(test.want) {
			t.Fatalf("%s: %q, %v", test.name, data, err)
		}
		if waited := time.Since(start); waited < back-20*time.Millisecond {
			t.Fatalf("%s didn't wait: %v", test.name, waited)
		}
	}
}

// TestOnlyGatewaysRest ensures a host that isn't a gateway has no rotation to leave.
func TestOnlyGatewaysRest(t *testing.T) {
	p := newGatewayPair(t)
	p.f.failed("stranger.example", "test")
	if len(p.f.out) != 0 {
		t.Fatal("a stranger's host was counted")
	}
}

// TestStallRestsGateway ensures a file that stalls takes its gateway out.
func TestStallRestsGateway(t *testing.T) {
	server, f := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
	})
	f.Gateways = []string{server.URL}
	if _, _, err := f.download(context.Background(), f.icons, server.URL+"/x", 1000); err == nil {
		t.Fatal("no error")
	}
	if _, ok := f.out[hostOf(server.URL)]; !ok {
		t.Fatal("a file that broke off left the gateway in")
	}
}

// TestMissingFilesRestNoGateway ensures files that no gateway gives, as ones
// nobody pins, rest none of them, however many there are and however often
// they're asked for, as it's the files that are missing.
func TestMissingFilesRestNoGateway(t *testing.T) {
	timeout := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
	}
	first, f := testServer(t, timeout)
	second := httptest.NewTLSServer(http.HandlerFunc(timeout))
	t.Cleanup(second.Close)
	f.Gateways = []string{first.URL, second.URL}
	f.roundPause = 0
	hosts := []string{hostOf(first.URL), hostOf(second.URL)}

	for _, uri := range []string{"ipfs://bafygone/icon.png", "ipfs://bafygone2/icon.png",
		"ipfs://bafygone/icon.png", "ipfs://bafygone3/icon.png"} {

		if _, err := f.FetchIcon(context.Background(), uri); err == nil {
			t.Fatalf("%s came", uri)
		}
		if inRotationLen(f, hosts...) != 2 {
			t.Fatalf("after %s, files no gateway has rested a gateway", uri)
		}
	}
	file := []byte(`{"r":1}`)
	if _, err := f.Fetch(context.Background(), sha256.Sum256(file), []string{"ipfs://bafygone/r.json"}); err == nil {
		t.Fatal("a registry no gateway has came")
	}
	if inRotationLen(f, hosts...) != 2 {
		t.Fatal("a registry no gateway has rested a gateway")
	}
}

// TestFaultOnFileAnotherGivesRests ensures a gateway that times out on a file
// that another gateway gives rests, as then it's the gateway that failed.
func TestFaultOnFileAnotherGivesRests(t *testing.T) {
	first, f := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
	})
	second := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("icon"))
	}))
	t.Cleanup(second.Close)
	f.Gateways = []string{first.URL, second.URL}
	f.roundPause = 0
	data, err := f.FetchIcon(context.Background(), "ipfs://bafy1/icon.png")
	if err != nil || string(data) != "icon" {
		t.Fatalf("%q, %v", data, err)
	}
	if inRotationLen(f, hostOf(first.URL)) != 0 {
		t.Fatal("a gateway that failed a file another gave is still in")
	}
	if inRotationLen(f, hostOf(second.URL)) != 1 {
		t.Fatal("the gateway that gave the file rests")
	}
}

// TestGatewayLeavingRotationLogsWhy ensures the log says why a gateway left
// rotation and for how long: a fault that counted as another gateway gave the
// item, a 429's pause, and the state of every gateway when the user is told that
// all are out.
func TestGatewayLeavingRotationLogsWhy(t *testing.T) {
	first, f := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
	})
	second := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "900")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(second.Close)
	third := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("icon"))
	}))
	t.Cleanup(third.Close)
	f.Gateways = []string{first.URL, second.URL, third.URL}
	f.roundPause = 0
	logged.take()

	data, err := f.FetchIcon(context.Background(), "ipfs://bafy1/icon.png")
	if err != nil || string(data) != "icon" {
		t.Fatalf("%q, %v", data, err)
	}
	got := logged.take()
	firstHost, secondHost, thirdHost := hostOf(first.URL), hostOf(second.URL), hostOf(third.URL)
	for _, want := range []string{
		"Gateway " + firstHost + " out of rotation for 30s, after 1 failures in a row: ",
		"504", ", while " + thirdHost + " gave the item",
		"Gateway " + secondHost + " paused for 15m0s, as it asked: ",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the log has no %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Gateway "+thirdHost) {
		t.Fatalf("the gateway that gave the item left rotation:\n%s", got)
	}

	// With the last one out too, an icon the user asks for says how each is.
	f.failed(thirdHost, "test")
	logged.take()
	if _, err := f.FetchIcon(UserAction(context.Background()), "ipfs://bafy2/icon.png"); !errors.Is(err, ErrNoGateway) {
		t.Fatal(err)
	}
	got = logged.take()
	for _, want := range []string{
		"Every gateway of an icon the user asked for is paused or out: ",
		firstHost + " out 30s more after 1 failures",
		secondHost + " paused 15m0s more",
		thirdHost + " out 30s more after 1 failures",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the log has no %q:\n%s", want, got)
		}
	}
}

// TestFetchSkipsGatewayOut ensures a gateway out of rotation isn't asked until it's
// back, while another is.
func TestFetchSkipsGatewayOut(t *testing.T) {
	p := newGatewayPair(t)
	p.status.Store(http.StatusBadGateway)
	if got := p.icon(t); got != "second" {
		t.Fatalf("%s", got)
	}
	for range 3 {
		if got := p.icon(t); got != "second" {
			t.Fatalf("%s", got)
		}
	}
	if n := p.hits.Load(); n != 1 {
		t.Fatalf("asked %d times while out", n)
	}
	p.pass(gatewayRestFirst)
	p.status.Store(http.StatusOK)
	if got := p.icon(t); got != "first" {
		t.Fatalf("%s", got)
	}
}

// TestRateLimitPauses ensures a gateway that answers 429 is paused for exactly as
// long as it asks, in seconds or until a date, however long, and for the steps of
// a failure if it doesn't say.
func TestRateLimitPauses(t *testing.T) {
	for _, test := range []struct {
		header string
		rest   time.Duration
	}{
		{"900", 900 * time.Second},
		{"86400", 24 * time.Hour},
		{"", gatewayRestFirst},
		{"soon", gatewayRestFirst},
		{"-5", gatewayRestFirst},
		{"date", 2 * time.Hour},
	} {
		p := newGatewayPair(t)
		header := test.header
		if header == "date" {
			header = p.f.now().Add(2 * time.Hour).UTC().Format(http.TimeFormat)
		}
		if header != "" {
			p.retryAfter.Store(&header)
		}
		p.status.Store(http.StatusTooManyRequests)
		if got := p.icon(t); got != "second" {
			t.Fatalf("%q: %s", test.header, got)
		}
		p.pass(test.rest - time.Second)
		if inRotationLen(p.f, p.first, "x") != 1 {
			t.Fatalf("%q: back before %v", test.header, test.rest)
		}
		p.pass(time.Second)
		if inRotationLen(p.f, p.first, "x") != 2 {
			t.Fatalf("%q: not back after %v", test.header, test.rest)
		}
	}
}

// TestRateLimitOnlyForGateways ensures a stranger's 429 rests nothing.
func TestRateLimitOnlyForGateways(t *testing.T) {
	p := newGatewayPair(t)
	p.f.pause("stranger.example", time.Hour, "test")
	if len(p.f.out) != 0 {
		t.Fatal("a stranger's host was paused")
	}
}

// TestFirstCopyWinsForAll ensures that of two requests for one item, the first to get
// the file ends the other, which returns it too, while a request for another item
// goes on.
func TestFirstCopyWinsForAll(t *testing.T) {
	var asked atomic.Int32
	stuck := make(chan struct{})
	server, f := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		// The first request for an icon is slow: it waits until it's given up.
		if r.URL.Path == "/icon.png" && asked.Add(1) == 1 {
			close(stuck)
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte("icon " + r.URL.Path))
	})
	slow := make(chan []byte, 1)
	go func() {
		data, err := f.FetchIcon(context.Background(), server.URL+"/icon.png")
		if err != nil {
			t.Error(err)
		}
		slow <- data
	}()
	<-stuck
	data, err := f.FetchIcon(context.Background(), server.URL+"/icon.png")
	if err != nil || string(data) != "icon /icon.png" {
		t.Fatalf("%q, %v", data, err)
	}
	select {
	case got := <-slow:
		if string(got) != "icon /icon.png" {
			t.Fatalf("the slow copy got %q", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the slow copy wasn't ended")
	}
	if len(f.items) != 0 {
		t.Fatal("an item was kept")
	}
}

// TestItemsKeptApart ensures items are told apart by what they are: registries by
// hash, icons by URI, and a failure for one leaves the others alone.
func TestItemsKeptApart(t *testing.T) {
	file := []byte(`{"r":1}`)
	server, f := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing.png" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(file)
	})
	if _, err := f.FetchIcon(context.Background(), server.URL+"/missing.png"); err == nil {
		t.Fatal("no error")
	}
	data, err := f.Fetch(context.Background(), sha256.Sum256(file), []string{server.URL + "/r"})
	if err != nil || string(data) != string(file) {
		t.Fatalf("%q, %v", data, err)
	}
	if len(f.items) != 0 {
		t.Fatal("an item was kept")
	}
}

// TestOtherItemsDontEndARequest ensures a request is only ended by its own item: an
// icon that's slow keeps going while another icon comes, and gets its own file.
func TestOtherItemsDontEndARequest(t *testing.T) {
	stuck := make(chan struct{})
	release := make(chan struct{})
	server, f := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/a.png" {
			close(stuck)
			<-release
		}
		_, _ = w.Write([]byte(r.URL.Path))
	})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	t.Cleanup(finish)
	slow := make(chan []byte, 1)
	go func() {
		data, _ := f.FetchIcon(context.Background(), server.URL+"/a.png")
		slow <- data
	}()
	<-stuck
	if data, err := f.FetchIcon(context.Background(), server.URL+"/b.png"); err != nil || string(data) != "/b.png" {
		t.Fatalf("%q, %v", data, err)
	}
	select {
	case got := <-slow:
		t.Fatalf("another icon ended the request, with %q", got)
	case <-time.After(50 * time.Millisecond):
	}
	finish()
	if got := <-slow; string(got) != "/a.png" {
		t.Fatalf("%q", got)
	}
}

// TestAskingAgainTriesAnotherGateway ensures a second request for an item that's slow
// at one gateway goes to another, though the slow one has room, and that with no
// other gateway it goes to the same one.
func TestAskingAgainTriesAnotherGateway(t *testing.T) {
	stuck := make(chan struct{})
	ended := make(chan struct{})
	slow := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case stuck <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-ended:
		}
	}))
	t.Cleanup(slow.Close)
	// Before the server closes, which waits for its handlers, if this fails.
	t.Cleanup(func() { close(ended) })
	fast, f := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("fast"))
	})
	f.Gateways = []string{slow.URL, fast.URL}
	first := make(chan []byte, 1)
	go func() {
		data, _ := f.FetchIcon(context.Background(), "ipfs://bafy1/icon.png")
		first <- data
	}()
	<-stuck
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	data, err := f.FetchIcon(ctx, "ipfs://bafy1/icon.png")
	if err != nil || string(data) != "fast" {
		t.Fatalf("asked again, it went to the slow gateway: %q, %v", data, err)
	}
	if got := <-first; string(got) != "fast" {
		t.Fatalf("%q", got)
	}

	// Alone at the gateway, a second request still goes there.
	if hosts := f.avoidOthers(context.Background(), []string{"a"}); len(hosts) != 1 {
		t.Fatalf("%v", hosts)
	}
	item := &sharedItem{hosts: map[string]int{"a": 1}}
	ctx = context.WithValue(context.Background(), sharedItemKey{}, item)
	if hosts := f.avoidOthers(ctx, []string{"a", "b"}); len(hosts) != 1 || hosts[0] != "b" {
		t.Fatalf("%v", hosts)
	}
	if hosts := f.avoidOthers(ctx, []string{"a"}); len(hosts) != 1 || hosts[0] != "a" {
		t.Fatalf("%v", hosts)
	}
}

// TestUserActionsGoFirst ensures what the user asked for goes ahead of all that loads
// by itself in a host's line, and that users are served in the order they came.
func TestUserActionsGoFirst(t *testing.T) {
	s := newHostSlots(1)
	held, _ := s.take(context.Background(), "h")
	order := make(chan string, 4)
	inLine := func(n int) {
		for {
			s.mu.Lock()
			l := len(s.waiting)
			s.mu.Unlock()
			if l == n {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	var all sync.WaitGroup
	for i, name := range []string{"background 1", "user 1", "background 2", "user 2"} {
		ctx := context.Background()
		if strings.HasPrefix(name, "user") {
			ctx = UserAction(ctx)
		}
		all.Add(1)
		go func() {
			defer all.Done()
			release, err := s.take(ctx, "h")
			if err == nil {
				order <- name
				release()
			}
		}()
		inLine(i + 1)
	}
	held()
	all.Wait()
	close(order)
	var got []string
	for name := range order {
		got = append(got, name)
	}
	want := []string{"user 1", "user 2", "background 1", "background 2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

// skipRests has a gateway's rests pass at once, as the clock moves on 10
// minutes each time it's read, for tests whose rounds would wait them out.
func skipRests(f *Fetcher) {
	var mu sync.Mutex
	clock := time.Now()
	f.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(10 * time.Minute)
		return clock
	}
}

// TestLocationTriedOnce ensures a publisher's URL that is also a gateway's location
// is tried once a round, rather than again after waiting for the gateway it failed
// at.
func TestLocationTriedOnce(t *testing.T) {
	var asked atomic.Int32
	server, f := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	})
	f.Gateways = []string{server.URL}
	skipRests(f)
	start := time.Now()
	_, err := f.Fetch(context.Background(), sha256.Sum256([]byte("x")), []string{server.URL + "/ipfs/bafkrei1"})
	if !errors.Is(err, ErrRegistryUnavailable) {
		t.Fatalf("%v", err)
	}
	if asked.Load() != 3 || time.Since(start) > 10*time.Second {
		t.Fatalf("asked %d times in %v", asked.Load(), time.Since(start))
	}
}

// endsWhenCancelled is a transport whose file sends nothing, then ends cleanly
// when the request is ended, as a host that stops sending can.
type endsWhenCancelled struct{}

func (endsWhenCancelled) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       cleanEnd{req.Context()},
		Request:    req,
	}, nil
}

type cleanEnd struct{ ctx context.Context }

func (b cleanEnd) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, io.EOF
}

func (cleanEnd) Close() error { return nil }

// TestStalledFileThatEndsIsAFailure ensures a download ended for being too slow
// fails, though the file then ends cleanly, rather than pass as the whole of it.
func TestStalledFileThatEndsIsAFailure(t *testing.T) {
	old := rateWindow
	rateWindow = time.Second
	defer func() { rateWindow = old }()
	f := newFetcher(nil, "", endsWhenCancelled{})
	if data, _, err := f.download(context.Background(), f.registries, "https://stalls.example/r", 100); err == nil {
		t.Fatalf("passed with %q", data)
	}
}

// notFoundFirst returns a handler that answers 404 to the first misses requests,
// as a gateway still looking for a file does, then serves data, and counts them.
func notFoundFirst(misses int32, data []byte, hits *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) <= misses {
			http.NotFound(w, nil)
			return
		}
		_, _ = w.Write(data)
	}
}

// TestFetchRounds ensures a registry that no location gave is asked for again,
// up to three rounds in all.
func TestFetchRounds(t *testing.T) {
	good := []byte(`{"good":true}`)
	var hits atomic.Int32
	server, f := testServer(t, notFoundFirst(2, good, &hits))
	data, err := f.Fetch(context.Background(), sha256.Sum256(good), []string{server.URL + "/r"})
	if err != nil || string(data) != string(good) || hits.Load() != 3 {
		t.Fatalf("got %q, %v after %d", data, err, hits.Load())
	}

	hits.Store(0)
	server, f = testServer(t, notFoundFirst(100, good, &hits))
	_, err = f.Fetch(context.Background(), sha256.Sum256(good), []string{server.URL + "/r"})
	if !errors.Is(err, ErrRegistryUnavailable) || hits.Load() != 3 {
		t.Fatalf("got %v after %d", err, hits.Load())
	}
}

// TestFetchIconRounds ensures an icon that no location gave is asked for again.
func TestFetchIconRounds(t *testing.T) {
	var hits atomic.Int32
	server, f := testServer(t, notFoundFirst(1, []byte("icon"), &hits))
	data, err := f.FetchIcon(context.Background(), server.URL+"/icon.png")
	if err != nil || string(data) != "icon" || hits.Load() != 2 {
		t.Fatalf("got %q, %v after %d", data, err, hits.Load())
	}

	hits.Store(0)
	server, f = testServer(t, notFoundFirst(100, nil, &hits))
	_, err = f.FetchIcon(context.Background(), server.URL+"/icon.png")
	if !errors.Is(err, ErrIconUnavailable) || hits.Load() != 3 {
		t.Fatalf("got %v after %d", err, hits.Load())
	}
}

// TestFetchRoundsWait ensures rounds are a pause apart, and that a request ended
// during one ends at once.
func TestFetchRoundsWait(t *testing.T) {
	var hits atomic.Int32
	server, f := testServer(t, notFoundFirst(100, nil, &hits))
	f.roundPause = 200 * time.Millisecond
	start := time.Now()
	_, _ = f.FetchIcon(context.Background(), server.URL+"/icon.png")
	if took := time.Since(start); took < 2*f.roundPause {
		t.Fatalf("three rounds in %v", took)
	}

	f.roundPause = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for hits.Load() < 4 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	_, err := f.FetchIcon(ctx, server.URL+"/icon.png")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

// TestFetchNoRoundsForWrongFiles ensures a file that isn't the one asked for gets
// no more rounds, as its location would only give it again, nor does a registry
// whose host serves another file, with the other locations failing.
func TestFetchNoRoundsForWrongFiles(t *testing.T) {
	var hits atomic.Int32
	server, f := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"tampered":true}`))
	})
	hash := sha256.Sum256([]byte("good"))
	data, err := f.Fetch(context.Background(), hash, []string{server.URL + "/bad", server.URL + "/missing"})
	if !errors.Is(err, ErrRegistryChanged) || data == nil || hits.Load() != 2 {
		t.Fatalf("got %q, %v after %d", data, err, hits.Load())
	}

	hits.Store(0)
	f.Gateways = []string{server.URL}
	if _, err := f.FetchIcon(context.Background(), rawCID([]byte("icon"))); !errors.Is(err, ErrIconUnavailable) ||
		hits.Load() != 1 {

		t.Fatalf("got %v after %d", err, hits.Load())
	}
}

// TestFetcherUnavailable ensures URIs count as unavailable only when every
// location they have is at a host out of rotation, and that URIs with none
// don't.
func TestFetcherUnavailable(t *testing.T) {
	f := newFetcher([]string{"https://gw1.example", "https://gw2.example"}, "", nil)
	out := func(host string) {
		f.healthMu.Lock()
		f.out[host] = gatewayOut{failures: 1, until: time.Now().Add(time.Hour)}
		f.healthMu.Unlock()
	}
	uris := []string{"https://host.example/ipfs/bafkrei1", "ipfs://bafkrei1"}
	if f.Unavailable(uris) {
		t.Fatal("unavailable with every host in rotation")
	}
	out("gw1.example")
	out("gw2.example")
	if f.Unavailable(uris) {
		t.Fatal("unavailable with the publisher's host in rotation")
	}
	out("host.example")
	if !f.Unavailable(uris) {
		t.Fatal("available with every host out")
	}
	if f.Unavailable([]string{"ftp://nothing.example"}) {
		t.Fatal("unavailable with no location")
	}
}

// slotsWithRotation returns slots of one at each host, and what takes a host out
// of rotation.
func slotsWithRotation() (*hostSlots, func(host string)) {
	s := newHostSlots(1)
	var mu sync.Mutex
	out := make(map[string]bool)
	s.usable = func(host string) bool {
		mu.Lock()
		defer mu.Unlock()
		return !out[host]
	}
	return s, func(host string) {
		mu.Lock()
		out[host] = true
		mu.Unlock()
	}
}

// waitInLine waits for the slots to have n waiting.
func waitInLine(t *testing.T, s *hostSlots, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		s.mu.Lock()
		in := len(s.waiting)
		s.mu.Unlock()
		if in == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d waiting, want %d", in, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestWaitMovesToGatewayBack ensures a download waiting in line for a slot at
// a busy gateway takes one at a gateway of the item that rested when it joined,
// once that's back, rather than waiting on the busy one for good.
func TestWaitMovesToGatewayBack(t *testing.T) {
	busy, f := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("busy"))
	})
	back := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("back"))
	}))
	t.Cleanup(back.Close)
	f.Gateways = []string{busy.URL, back.URL}

	// Every slot at the first is taken, and the second rests a while.
	var releases []func()
	for range maxPerHost {
		_, release, err := f.icons.takeAny(context.Background(), []string{hostOf(busy.URL)})
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	const rest = 200 * time.Millisecond
	f.healthMu.Lock()
	f.out[hostOf(back.URL)] = gatewayOut{failures: 1, until: time.Now().Add(rest)}
	f.healthMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	data, _, location, err := f.downloadAny(ctx, f.icons,
		[]string{busy.URL + "/ipfs/bafy1", back.URL + "/ipfs/bafy1"}, maxIconSize)
	if err != nil || string(data) != "back" || !strings.HasPrefix(location, back.URL) {
		t.Fatalf("got %q from %s: %v", data, location, err)
	}
}

// TestSlotsSkipHostOutOfRotation ensures a slot freed at a host out of rotation
// isn't handed on, while one at a host in rotation is.
func TestSlotsSkipHostOutOfRotation(t *testing.T) {
	s, takeOut := slotsWithRotation()
	_, releaseA, _ := s.takeAny(context.Background(), []string{"a"})
	_, releaseB, _ := s.takeAny(context.Background(), []string{"b"})
	got := make(chan string, 1)
	go func() {
		host, release, err := s.takeAny(context.Background(), []string{"a", "b"})
		if err == nil {
			release()
		}
		got <- host
	}()
	waitInLine(t, s, 1)
	takeOut("a")
	releaseA()
	s.mu.Lock()
	busy, waiting := s.busy["a"], len(s.waiting)
	s.mu.Unlock()
	if busy != 0 || waiting != 1 {
		t.Fatalf("a slot at a host out of rotation went on: %d busy, %d waiting", busy, waiting)
	}
	releaseB()
	select {
	case host := <-got:
		if host != "b" {
			t.Fatalf("took %q", host)
		}
	case <-time.After(time.Minute):
		t.Fatal("a slot at a host in rotation didn't go on")
	}
}

// TestSlotsRecheck ensures a wait whose hosts all left rotation ends, so that it
// goes back to waiting for one to come back, and that a wait with a host left in
// rotation goes on.
func TestSlotsRecheck(t *testing.T) {
	s, takeOut := slotsWithRotation()
	_, releaseA, _ := s.takeAny(context.Background(), []string{"a"})
	defer releaseA()
	_, releaseB, _ := s.takeAny(context.Background(), []string{"b"})
	defer releaseB()
	onlyA := make(chan error, 1)
	go func() {
		_, _, err := s.takeAny(context.Background(), []string{"a"})
		onlyA <- err
	}()
	waitInLine(t, s, 1)
	ctx, cancel := context.WithCancel(context.Background())
	either := make(chan error, 1)
	go func() {
		_, _, err := s.takeAny(ctx, []string{"a", "b"})
		either <- err
	}()
	waitInLine(t, s, 2)
	takeOut("a")
	s.recheck()
	select {
	case err := <-onlyA:
		if !errors.Is(err, errLeftRotation) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("a wait whose hosts all left rotation went on")
	}
	waitInLine(t, s, 1)
	cancel()
	if err := <-either; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

// TestFetchWaiterSeesGatewayLeave ensures an icon the user asked for, waiting for
// a slot at a gateway that then leaves rotation, as one that fails or is rate
// limited does, is told at once that every gateway is out, rather than wait for
// a slot there.
func TestFetchWaiterSeesGatewayLeave(t *testing.T) {
	leave := map[string]func(f *Fetcher, host string){
		"failed": func(f *Fetcher, host string) { f.failed(host, "test") },
		"paused": func(f *Fetcher, host string) { f.pause(host, time.Hour, "test") },
	}
	for name, leave := range leave {
		arrived := make(chan struct{}, maxPerHost)
		release := make(chan struct{})
		server, f := testServer(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "held") {
				arrived <- struct{}{}
				<-release
			}
			_, _ = w.Write([]byte("icon"))
		})
		f.Gateways = []string{server.URL}
		for i := range maxPerHost {
			go func() { _, _ = f.FetchIcon(context.Background(), fmt.Sprintf("ipfs://held%d", i)) }()
			<-arrived
		}
		got := make(chan error, 1)
		go func() {
			_, err := f.FetchIcon(UserAction(context.Background()), "ipfs://wanted")
			got <- err
		}()
		waitInLine(t, f.icons, 1)
		leave(f, hostOf(server.URL))
		select {
		case err := <-got:
			if !errors.Is(err, ErrNoGateway) {
				t.Fatalf("%s: got %v", name, err)
			}
		case <-time.After(time.Minute):
			t.Fatalf("%s: the waiter wasn't told its gateway left", name)
		}
		close(release)
	}
}

// TestFetchPassesOverOutLocations ensures a location whose hosts are all out of
// rotation is passed over for the next, as an icon at a gateway that's out is
// read at another, and a registry whose gateways are all out from its own host,
// rather than end the round or wait there.
func TestFetchPassesOverOutLocations(t *testing.T) {
	icon := []byte("icon")
	registry := []byte(`{"r":1}`)
	serve := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "icon.png") {
			_, _ = w.Write(icon)
			return
		}
		_, _ = w.Write(registry)
	}
	paused, f := testServer(t, serve)
	open := httptest.NewTLSServer(http.HandlerFunc(serve))
	t.Cleanup(open.Close)
	own := httptest.NewTLSServer(http.HandlerFunc(serve))
	t.Cleanup(own.Close)
	f.Gateways = []string{paused.URL, open.URL}
	f.pause(hostOf(paused.URL), time.Hour, "test")

	data, err := f.FetchIcon(UserAction(context.Background()), paused.URL+"/ipfs/bafy1/icon.png")
	if err != nil || string(data) != "icon" {
		t.Fatalf("icon: %q, %v", data, err)
	}

	f.pause(hostOf(open.URL), time.Hour, "test")
	data, err = f.Fetch(context.Background(), sha256.Sum256(registry),
		[]string{"ipfs://bafy2", own.URL + "/registry.json"})
	if err != nil || string(data) != string(registry) {
		t.Fatalf("registry: %q, %v", data, err)
	}
}

// TestFetchWrongPublicationSparesGateways ensures a registry whose publication
// names a hash that isn't its CID's file leaves the gateways that served that
// file in rotation, while one that serves other than its CID names is rested.
func TestFetchWrongPublicationSparesGateways(t *testing.T) {
	file := []byte(`{"r":1}`)
	cid := strings.TrimPrefix(rawCID(file), "ipfs://")
	serve := file
	server, f := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(serve)
	})
	f.Gateways = []string{server.URL}
	host := hostOf(server.URL)
	wrong := sha256.Sum256([]byte("another file"))
	if _, err := f.Fetch(context.Background(), wrong, []string{"ipfs://" + cid}); err == nil {
		t.Fatal("no error")
	}
	if !f.available(host) {
		t.Fatal("a gateway that served the CID's file was rested")
	}

	serve = []byte(`{"r":2}`)
	if _, err := f.Fetch(context.Background(), sha256.Sum256(file), []string{"ipfs://" + cid}); err == nil {
		t.Fatal("no error")
	}
	if f.available(host) {
		t.Fatal("a gateway that served another file than its CID's stayed in rotation")
	}
}

// TestFailsItsCID ensures only a file at a raw CID's path that isn't that CID's
// file counts against the location.
func TestFailsItsCID(t *testing.T) {
	file := []byte("file")
	cid := strings.TrimPrefix(rawCID(file), "ipfs://")
	tests := []struct {
		location string
		data     []byte
		want     bool
	}{
		{"https://gw.example/ipfs/" + cid, file, false},
		{"https://gw.example/ipfs/" + cid, []byte("other"), true},
		{"https://gw.example/ipfs/bafybeidirectory/1.json", []byte("other"), false},
		{"https://host.example/registry.json", []byte("other"), false},
	}
	for _, test := range tests {
		if got := failsItsCID(test.location, test.data); got != test.want {
			t.Errorf("%s: got %v", test.location, got)
		}
	}
}

// TestFetchWrongFilesAddUp ensures a gateway that keeps serving other files than
// the CIDs name fails each time in a row, rather than start again at each file
// it sends, and that one it serves right puts it back.
func TestFetchWrongFilesAddUp(t *testing.T) {
	icon := []byte("icon")
	serve := []byte("tampered")
	server, f := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(serve)
	})
	f.Gateways = []string{server.URL}
	skipRests(f)
	host := hostOf(server.URL)
	for range 2 {
		if _, err := f.FetchIcon(context.Background(), rawCID(icon)); err == nil {
			t.Fatal("no error")
		}
	}
	f.healthMu.Lock()
	failures := f.out[host].failures
	f.healthMu.Unlock()
	if failures != 2 {
		t.Fatalf("%d failures in a row", failures)
	}

	serve = icon
	if _, err := f.FetchIcon(context.Background(), rawCID(icon)); err != nil {
		t.Fatal(err)
	}
	f.healthMu.Lock()
	_, out := f.out[host]
	f.healthMu.Unlock()
	if out {
		t.Fatal("a right file didn't put the gateway back")
	}

	// A registry it serves right does too.
	f.failed(host, "test")
	registry := []byte(`{"r":1}`)
	serve = registry
	if _, err := f.Fetch(context.Background(), sha256.Sum256(registry), []string{rawCID(registry)}); err != nil {
		t.Fatal(err)
	}
	f.healthMu.Lock()
	_, out = f.out[host]
	f.healthMu.Unlock()
	if out {
		t.Fatal("a right registry didn't put the gateway back")
	}
}

// TestFetchWaitsOnlyWhenAllOut ensures a round that tried a location, which
// failed, while the others were out of rotation, fails, rather than wait for them.
func TestFetchWaitsOnlyWhenAllOut(t *testing.T) {
	gateway, f := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("never asked"))
	})
	own := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(own.Close)
	f.Gateways = []string{gateway.URL}
	f.pause(hostOf(gateway.URL), time.Hour, "test")
	done := make(chan error, 1)
	go func() {
		_, err := f.Fetch(context.Background(), sha256.Sum256([]byte("r")),
			[]string{"ipfs://bafy1", own.URL + "/registry.json"})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrRegistryUnavailable) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("a round with a location tried waited for those out of rotation")
	}
}

// TestFetchSuccessLeavesPause ensures a download that ends well, as one under
// way when the gateway asked to be left alone does, counts its failures as over
// but leaves the pause the gateway asked for, and that one with no pause puts
// the gateway back.
func TestFetchSuccessLeavesPause(t *testing.T) {
	f := newFetcher([]string{"https://gw.example"}, "", nil)
	f.failed("gw.example", "test")
	f.pause("gw.example", time.Hour, "test")
	f.worked("gw.example")
	if f.available("gw.example") {
		t.Fatal("a success ended a pause")
	}
	f.healthMu.Lock()
	failures := f.out["gw.example"].failures
	f.healthMu.Unlock()
	if failures != 0 {
		t.Fatalf("%d failures after a success", failures)
	}

	f.out = make(map[string]gatewayOut)
	f.failed("gw.example", "test")
	f.worked("gw.example")
	if !f.available("gw.example") {
		t.Fatal("a success didn't end a rest")
	}
}

// locations returns the locations of a URI, as locationGroups has them, in one
// list.
func (f *Fetcher) locations(uri string) []string {
	var locations []string
	for _, group := range f.locationGroups(uri) {
		locations = append(locations, group...)
	}
	return locations
}

// download reads a URL as downloadAny does, alone.
func (f *Fetcher) download(ctx context.Context, slots *hostSlots, location string,
	limit int64) ([]byte, *url.URL, error) {

	data, final, _, err := f.downloadAny(ctx, slots, []string{location}, limit)
	return data, final, err
}

// take waits for a slot at the host, and returns what gives it back.
func (s *hostSlots) take(ctx context.Context, host string) (func(), error) {
	_, release, err := s.takeAny(ctx, []string{host})
	return release, err
}
