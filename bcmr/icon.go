package bcmr

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
)

// maxIconSize bounds an icon.
const maxIconSize = 1 << 20

// ErrIconUnavailable is the error of an icon no source gave.
var ErrIconUnavailable = errors.New("no source gave the icon")

// ipfsRawDigest returns the SHA-256 that an ipfs URI of a raw file names, and
// false for any other URI.  Such a URI names its file's hash, as bafkrei... does:
// a base32 CIDv1 of the raw codec with a SHA-256 multihash.  An icon behind any
// other URI can change, so it can't be checked.
func ipfsRawDigest(uri string) ([32]byte, bool) {
	var digest [32]byte
	rest, ok := strings.CutPrefix(uri, "ipfs://")
	if !ok || !strings.HasPrefix(rest, "b") || strings.Contains(rest, "/") {
		return digest, false
	}
	encoding := base32.StdEncoding.WithPadding(base32.NoPadding)
	raw, err := encoding.DecodeString(strings.ToUpper(rest[1:]))
	if err != nil || len(raw) != 4+len(digest) {
		return digest, false
	}
	if !bytes.Equal(raw[:4], []byte{0x01, 0x55, 0x12, 0x20}) {
		return digest, false
	}
	copy(digest[:], raw[4:])
	return digest, true
}

// FetchIcon returns the file of an icon's URI, checked against the hash an ipfs
// URI names, if it names one.  Connections have the limits of a registry's, and
// a smaller size.
func (f *Fetcher) FetchIcon(ctx context.Context, uri string) ([]byte, error) {
	digest, checked := ipfsRawDigest(uri)
	var last error
	for _, location := range f.locations(uri) {
		data, _, err := f.download(ctx, location)
		switch {
		case err != nil:
		case len(data) > maxIconSize:
			err = fmt.Errorf("%s is over %d bytes", location, maxIconSize)
		case checked && sha256.Sum256(data) != digest:
			err = fmt.Errorf("%s doesn't match the hash of the URI", location)
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = err
			continue
		}
		return data, nil
	}
	if last != nil {
		return nil, fmt.Errorf("%w: %v", ErrIconUnavailable, last)
	}
	return nil, ErrIconUnavailable
}

// Icon returns the file of an icon's URI.  It's only for icons the user chose
// to load, as a host sees who asks.
func (m *Manager) Icon(ctx context.Context, uri string) ([]byte, error) {
	return m.cfg.Fetcher.FetchIcon(ctx, uri)
}
