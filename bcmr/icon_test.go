package bcmr

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"net/http"
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
	server, f := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/bad") {
			_, _ = w.Write([]byte("something else"))
			return
		}
		_, _ = w.Write(good)
	})
	f.Gateways = []string{server.URL + "/bad", server.URL + "/good"}
	data, err := f.FetchIcon(context.Background(), rawCID(good))
	if err != nil || string(data) != string(good) {
		t.Fatalf("got %q, %v", data, err)
	}
	f.Gateways = []string{server.URL + "/bad"}
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
