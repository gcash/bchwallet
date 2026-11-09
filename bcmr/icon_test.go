package bcmr

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rawCID returns the ipfs URI of a raw file with the given contents.
func rawCID(data []byte) string {
	digest := sha256.Sum256(data)
	raw := append([]byte{0x01, 0x55, 0x12, 0x20}, digest[:]...)
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	return "ipfs://b" + strings.ToLower(encoded)
}

// TestIPFSRawDigest ensures the hash of a raw CID is read, from a real one too,
// and that any other URI names none.
func TestIPFSRawDigest(t *testing.T) {
	digest, ok := ipfsRawDigest("ipfs://bafkreibsu7uoy37l2xg6kbir3id3t5xe4u4ysdkppof2k4pf6eusoc5mry")
	if !ok || digest[0] != 0x32 || digest[1] != 0xa7 || digest[2] != 0xe8 {
		t.Fatalf("got %x, %v", digest, ok)
	}
	for _, uri := range []string{
		"https://example.com/icon.png",
		"ipfs://QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG",
		"ipfs://bafkreibsu7uoy37l2xg6kbir3id3t5xe4u4ysdkppof2k4pf6eusoc5mry/icon.png",
		"ipfs://bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi",
		"ipfs://bnotbase32!",
		"ipfs://",
	} {
		if _, ok := ipfsRawDigest(uri); ok {
			t.Errorf("%s names a hash", uri)
		}
	}
}

// TestFetchIcon ensures an icon that doesn't match its CID is refused, and the
// next gateway tried, and that an https icon has nothing to match.
func TestFetchIcon(t *testing.T) {
	good := []byte("the real icon")
	server, f := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(good)
	})
	// A gateway of its own, as one that serves the wrong file rests.
	bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("something else"))
	}))
	t.Cleanup(bad.Close)
	f.Gateways = []string{bad.URL + "/bad", server.URL + "/good"}
	data, err := f.FetchIcon(context.Background(), rawCID(good))
	if err != nil || string(data) != string(good) {
		t.Fatalf("got %q, %v", data, err)
	}
	f.Gateways = []string{bad.URL + "/bad"}
	f.healthMu.Lock()
	f.out = make(map[string]gatewayOut)
	f.healthMu.Unlock()
	if _, err := f.FetchIcon(context.Background(), rawCID(good)); !errors.Is(err, ErrIconUnavailable) {
		t.Fatalf("got %v", err)
	}
	if _, err := f.FetchIcon(context.Background(), server.URL+"/good"); err != nil {
		t.Fatal(err)
	}
}

// TestFetchIconSize ensures an icon past its limit is refused.
func TestFetchIconSize(t *testing.T) {
	server, f := testServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, maxIconSize+1))
	})
	_, err := f.FetchIcon(context.Background(), server.URL+"/i")
	if !errors.Is(err, ErrIconUnavailable) || !strings.Contains(err.Error(), "over") {
		t.Fatalf("got %v", err)
	}
}

// TestFetchIconRefusesOtherSchemes ensures nothing is asked for a URI it can't
// use.
func TestFetchIconRefusesOtherSchemes(t *testing.T) {
	f := newFetcher(nil, "", nil)
	for _, uri := range []string{"http://example.com/i.png", "file:///etc/passwd", ""} {
		if _, err := f.FetchIcon(context.Background(), uri); !errors.Is(err, ErrIconUnavailable) {
			t.Errorf("%q: got %v", uri, err)
		}
	}
}

// TestFetchIconCIDAlone ensures an icon with a file name on a CID that's the file
// itself is found by its CID, with the hash the CID names checked.
func TestFetchIconCIDAlone(t *testing.T) {
	icon := []byte("icon")
	digest := sha256.Sum256(icon)
	raw := append([]byte{0x01, 0x55, 0x12, 0x20}, digest[:]...)
	cid := "b" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
	serve := icon
	server, f := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/"+cid) {
			_, _ = w.Write(serve)
			return
		}
		http.NotFound(w, r)
	})
	f.Gateways = []string{server.URL}
	skipRests(f)
	data, err := f.FetchIcon(context.Background(), "ipfs://"+cid+"/logo.png")
	if err != nil || string(data) != "icon" {
		t.Fatalf("got %q, %v", data, err)
	}
	serve = []byte("tampered")
	if _, err := f.FetchIcon(context.Background(), "ipfs://"+cid+"/logo.png"); !errors.Is(err, ErrIconUnavailable) {
		t.Fatalf("got %v", err)
	}
}

// TestFetchIconURLOfCID ensures an icon whose URL isn't ipfs:// but has the path of
// a raw CID is checked against it, as it's the same item as that CID's.
func TestFetchIconURLOfCID(t *testing.T) {
	cid := strings.TrimPrefix(rawCID([]byte("icon")), "ipfs://")
	serve := []byte("icon")
	server, f := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(serve)
	})
	skipRests(f)
	uri := server.URL + "/ipfs/" + cid
	if data, err := f.FetchIcon(context.Background(), uri); err != nil || string(data) != "icon" {
		t.Fatalf("got %q, %v", data, err)
	}
	serve = []byte("another token's icon")
	if _, err := f.FetchIcon(context.Background(), uri); !errors.Is(err, ErrIconUnavailable) {
		t.Fatalf("got %v", err)
	}
}

// TestIconItem ensures an icon is the same item whether its URI is ipfs:// or a
// gateway's URL of the same path, and another for another path or a plain URL.
func TestIconItem(t *testing.T) {
	ipfs := iconItem("ipfs://bafy1/icon.png")
	if iconItem("https://ipfs.io/ipfs/bafy1/icon.png") != ipfs ||
		iconItem("https://bafy1.ipfs.dweb.link/icon.png") != ipfs {

		t.Fatal("one path through a gateway's URL is another item")
	}
	if iconItem("ipfs://bafy1") == ipfs || iconItem("https://host.example/icon.png") == ipfs {
		t.Fatal("other icons are the same item")
	}
}
