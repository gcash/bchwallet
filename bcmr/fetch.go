package bcmr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	// maxRegistrySize bounds a download.  The largest registry in use is
	// about 5 MB.
	maxRegistrySize = 8 << 20

	// fetchTimeout bounds one download.
	fetchTimeout = 30 * time.Second

	// maxRedirects bounds the redirects a download follows.
	maxRedirects = 3

	// maxCacheSize is what the registries kept on disk can take up.
	maxCacheSize = 32 << 20

	// wellKnownPath is the CHIP's path for a URI that gives only a domain.
	wellKnownPath = "/.well-known/bitcoin-cash-metadata-registry.json"
)

var (
	// ErrRegistryUnavailable is the error of a registry none of whose URIs
	// gave the published file.
	ErrRegistryUnavailable = errors.New("no URI gave the registry")

	// errPrivateAddress refuses a connection to this device or its network.
	errPrivateAddress = errors.New("refusing a private address")
)

// Fetcher downloads registries, checks them against their published hashes,
// and keeps them by hash.  A hash that matches means the source didn't matter,
// so it can be any host or gateway.
type Fetcher struct {
	// Gateways are the HTTP gateways for ipfs URIs, tried in order, as
	// https://host with no path.
	Gateways []string

	// CacheDir is the directory for the registries that are kept.  A
	// Fetcher with none keeps nothing.
	CacheDir string

	client *http.Client
}

// NewFetcher returns a Fetcher.  Its connections refuse private addresses,
// since the URIs come from strangers, and must not reach this device or the
// network it's on.
//
// A dial that isn't nil replaces the connection, as a proxy does, so that a
// wallet behind one doesn't show a token's host where it is.  The proxy then
// resolves the name and decides where it leads.
func NewFetcher(gateways []string, cacheDir string,
	dial func(ctx context.Context, network, address string) (net.Conn, error)) *Fetcher {

	if dial == nil {
		dial = (&net.Dialer{Timeout: 10 * time.Second, Control: refusePrivate}).DialContext
	}
	return newFetcher(gateways, cacheDir, &http.Transport{
		DialContext:           dial,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		DisableKeepAlives:     true,
	})
}

func newFetcher(gateways []string, cacheDir string, transport http.RoundTripper) *Fetcher {
	return &Fetcher{
		Gateways: gateways,
		CacheDir: cacheDir,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > maxRedirects {
					return errors.New("too many redirects")
				}
				if req.URL.Scheme != "https" {
					return errors.New("redirect away from https")
				}
				return nil
			},
		},
	}
}

// refusePrivate refuses a connection to an address that isn't public.  It runs
// once the name is resolved, so a name can't hide such an address, and also
// after a redirect.
func refusePrivate(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !publicAddress(ip) {
		return fmt.Errorf("%w: %s", errPrivateAddress, host)
	}
	return nil
}

// nonPublicRanges are the ranges that Go's checks leave out but that don't lead
// to the public internet, as carrier-grade NAT, which tailnets use too, and the
// IPv6 prefixes that wrap an IPv4 address, which could be a private one.
var nonPublicRanges = mustParseCIDRs("0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24",
	"198.18.0.0/15", "240.0.0.0/4", "64:ff9b::/96", "2002::/16", "fec0::/10")

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	nets := make([]*net.IPNet, len(cidrs))
	for i, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(err)
		}
		nets[i] = network
	}
	return nets
}

// publicAddress returns whether ip is reachable on the public internet.
func publicAddress(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsMulticast() ||
		ip.IsUnspecified() {

		return false
	}
	for _, network := range nonPublicRanges {
		if network.Contains(ip) {
			return false
		}
	}
	return true
}

// Fetch returns the file with the published hash, from the cache or from the
// first of the URIs that gives it.
func (f *Fetcher) Fetch(ctx context.Context, hash [32]byte, uris []string) ([]byte, error) {
	if data := f.cached(hash); data != nil {
		return data, nil
	}
	var last error
	for _, uri := range uris {
		for _, location := range f.locations(uri) {
			data, err := f.download(ctx, location)
			if err == nil && sha256.Sum256(data) != hash {
				err = fmt.Errorf("%s doesn't match the published hash", location)
			}
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				last = err
				continue
			}
			f.keep(hash, data)
			return data, nil
		}
	}
	if last != nil {
		return nil, fmt.Errorf("%w: %v", ErrRegistryUnavailable, last)
	}
	return nil, ErrRegistryUnavailable
}

// locations returns the URLs to try for a URI, by the CHIP: no scheme means
// https, a domain alone means its well-known path, and ipfs goes through the
// gateways.  Any other scheme gives none.
func (f *Fetcher) locations(uri string) []string {
	if len(uri) > maxURILen || !displayable(uri, maxURILen) {
		return nil
	}
	if rest, ok := strings.CutPrefix(uri, "ipfs://"); ok {
		if rest == "" {
			return nil
		}
		locations := make([]string, 0, len(f.Gateways))
		for _, gateway := range f.Gateways {
			locations = append(locations, strings.TrimRight(gateway, "/")+"/ipfs/"+rest)
		}
		return locations
	}
	if strings.Contains(uri, "://") && !strings.HasPrefix(uri, "https://") {
		return nil
	}
	location := uri
	if !strings.HasPrefix(location, "https://") {
		location = "https://" + location
	}
	parsed, err := url.Parse(location)
	if err != nil || parsed.Host == "" {
		return nil
	}
	if parsed.Path == "" {
		parsed.Path = wellKnownPath
	}
	return []string{parsed.String()}
}

// download reads a URL, up to the size limit.
func (f *Fetcher) download(ctx context.Context, location string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", location, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRegistrySize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxRegistrySize {
		return nil, fmt.Errorf("%s is over %d bytes", location, maxRegistrySize)
	}
	return data, nil
}

// cachePath returns the file of a hash.
func (f *Fetcher) cachePath(hash [32]byte) string {
	return filepath.Join(f.CacheDir, hex.EncodeToString(hash[:]))
}

// cached returns the kept file of a hash, if it's there and intact.
func (f *Fetcher) cached(hash [32]byte) []byte {
	if f.CacheDir == "" {
		return nil
	}
	data, err := os.ReadFile(f.cachePath(hash))
	if err != nil || sha256.Sum256(data) != hash {
		return nil
	}
	return data
}

// keep stores a file by its hash, and trims the cache.  A failure only costs a
// download later.
func (f *Fetcher) keep(hash [32]byte, data []byte) {
	if f.CacheDir == "" || os.MkdirAll(f.CacheDir, 0700) != nil {
		return
	}
	path := f.cachePath(hash)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0600) != nil || os.Rename(tmp, path) != nil {
		_ = os.Remove(tmp)
		return
	}
	f.trim(hash)
}

// trim removes the oldest files until the cache fits, keeping the one just
// stored.
func (f *Fetcher) trim(keep [32]byte) {
	entries, err := os.ReadDir(f.CacheDir)
	if err != nil {
		return
	}
	type file struct {
		name string
		size int64
		time time.Time
	}
	var files []file
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, file{entry.Name(), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].time.Before(files[j].time) })
	kept := hex.EncodeToString(keep[:])
	for _, old := range files {
		if total <= maxCacheSize {
			return
		}
		if old.name == kept {
			continue
		}
		if os.Remove(filepath.Join(f.CacheDir, old.name)) == nil {
			total -= old.size
		}
	}
}
