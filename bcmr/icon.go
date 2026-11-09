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

// maxIconSize bounds an icon.  Many collections' icons are their full art, of up
// to about 6 MB, and the app scales them down.
const maxIconSize = 8 << 20

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
	return f.shared(ctx, "icon "+uri, func(ctx context.Context) ([]byte, error) {
		return f.fetchIcon(ctx, uri)
	})
}

// fetchIcon is FetchIcon for one copy of the request.
func (f *Fetcher) fetchIcon(ctx context.Context, uri string) ([]byte, error) {
	return f.inRounds(ctx, func() ([]byte, bool, error) {
		return f.fetchIconRound(ctx, uri)
	})
}

// fetchIconRound is one round of fetchIcon, and returns whether another may do
// better.  It passes over a location whose hosts are all out of rotation as
// fetchRound does.
func (f *Fetcher) fetchIconRound(ctx context.Context, uri string) ([]byte, bool, error) {
	for {
		data, again, out, err := f.fetchIconRoundOnce(ctx, uri)
		if out == nil {
			return data, again, err
		}
		if err := f.waitBack(ctx, f.icons, hostsOf(out)); err != nil {
			return nil, false, err
		}
	}
}

// fetchIconRoundOnce is a pass of fetchIconRound over the locations in rotation,
// and returns the locations out of it if they were all there was.
func (f *Fetcher) fetchIconRoundOnce(ctx context.Context, uri string) ([]byte, bool, []string, error) {
	var out []string
	var last error
	var again bool
	tried := make(map[string]bool)
	for _, candidate := range iconURIs(uri) {
		digest, checked := ipfsRawDigest(candidate)
		for _, group := range f.locationGroups(candidate) {
			left := untried(group, tried)
			if skipped := f.outOfRotation(left); skipped != nil {
				out = append(out, skipped...)
				continue
			}
			for len(left) > 0 {
				data, _, location, err := f.downloadAny(ctx, f.icons, left, maxIconSize)
				if location == "" {
					return nil, false, nil, err
				}
				tried[location] = true
				left = untried(left, tried)
				wrongFile := err == nil && checked && sha256.Sum256(data) != digest
				if wrongFile {
					err = fmt.Errorf("%s doesn't match the hash of the URI", location)
					f.failed(hostOf(location))
				}
				if err != nil {
					if ctx.Err() != nil {
						return nil, false, nil, ctx.Err()
					}
					last = err
					again = again || mayPass(err, wrongFile)
					continue
				}
				return data, false, nil, nil
			}
		}
	}
	if len(tried) == 0 && out != nil {
		return nil, false, out, ErrIconUnavailable
	}
	if last != nil {
		return nil, again, nil, fmt.Errorf("%w: %v", ErrIconUnavailable, last)
	}
	return nil, false, nil, ErrIconUnavailable
}

// iconURIs returns the URIs to try for an icon.  An ipfs URI with a file name on
// a CID that's the file itself, as Gambling Apes Club and Punk Apes Club publish,
// finds nothing, so its CID alone follows it.
func iconURIs(uri string) []string {
	rest, ok := strings.CutPrefix(uri, "ipfs://")
	if !ok {
		return []string{uri}
	}
	if cid, _, found := strings.Cut(rest, "/"); found && cid != "" {
		return []string{uri, "ipfs://" + cid}
	}
	return []string{uri}
}

// Icon returns the file of an icon's URI.  It's only for icons the user chose
// to load, as a host sees who asks.
func (m *Manager) Icon(ctx context.Context, uri string) ([]byte, error) {
	return m.cfg.Fetcher.FetchIcon(ctx, uri)
}
