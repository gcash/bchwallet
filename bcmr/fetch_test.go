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
		uris[i] = server.URL + "/r"
	}
	if _, err := f.Fetch(context.Background(), [32]byte{1}, uris); !errors.Is(err, ErrRegistryUnavailable) {
		t.Fatalf("got %v", err)
	}
	if hits.Load() != maxLocations {
		t.Fatalf("%d tries", hits.Load())
	}
}

// TestRefuseLocal ensures a connection through a proxy can't name this device or
// its network.
func TestRefuseLocal(t *testing.T) {
	for address, want := range map[string]bool{"localhost:443": true, "127.0.0.1:443": true,
		"10.0.0.1:443": true, "[::1]:443": true, "localhost.:443": true, "a.localhost:443": true,
		"127.1:443": true, "2130706433:443": true, "0x7f.1:443": true,
		"example.com:443": false, "8.8.8.8:443": false} {
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

	// A wait that can only give downloads over the limit time to show.
	time.Sleep(50 * time.Millisecond)
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
		if p.f.inRotation([]string{p.first})[0] != p.first || len(p.f.inRotation([]string{p.first, "x"})) != 1 {
			t.Fatalf("back before %v", rest)
		}
		p.pass(time.Second)
		if in := p.f.inRotation([]string{p.first, "x"}); len(in) != 2 {
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

// TestAllGatewaysOutStillTried ensures gateways that are all out are tried anyway,
// as there's nothing else.
func TestAllGatewaysOutStillTried(t *testing.T) {
	p := newGatewayPair(t)
	for _, gateway := range p.f.Gateways {
		p.f.failed(hostOf(gateway))
	}
	if got := p.icon(t); got != "first" {
		t.Fatalf("%s", got)
	}
}

// TestOnlyGatewaysRest ensures a host that isn't a gateway has no rotation to leave.
func TestOnlyGatewaysRest(t *testing.T) {
	p := newGatewayPair(t)
	p.f.failed("stranger.example")
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
		if len(p.f.inRotation([]string{p.first, "x"})) != 1 {
			t.Fatalf("%q: back before %v", test.header, test.rest)
		}
		p.pass(time.Second)
		if len(p.f.inRotation([]string{p.first, "x"})) != 2 {
			t.Fatalf("%q: not back after %v", test.header, test.rest)
		}
	}
}

// TestRateLimitOnlyForGateways ensures a stranger's 429 rests nothing.
func TestRateLimitOnlyForGateways(t *testing.T) {
	p := newGatewayPair(t)
	p.f.pause("stranger.example", time.Hour)
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
