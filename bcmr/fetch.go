package bcmr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// maxRegistrySize bounds a download.  The largest registry in use, of a
	// collection of 10,000 NFTs, is about 10 MB.
	maxRegistrySize = 16 << 20

	// maxLocations is how many locations a Fetch tries, as the URIs come from
	// strangers and a publication can hold many, and other tokens wait for it.
	maxLocations = 12

	// minDownloadRate is the slowest a download can be, in bytes a second over
	// rateWindow.
	minDownloadRate = 1024

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

	// ErrRegistryChanged is the error of a registry whose host gave a file
	// other than the published one.  Fetch returns the file with it.
	ErrRegistryChanged = errors.New("the registry isn't the published one")

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

	// keepMu lets one download at a time change the cache, as trimming it
	// counts and removes the files there.
	keepMu sync.Mutex
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

// rateWindow is how long a download has to reach minDownloadRate, a variable for
// the tests.
var rateWindow = 30 * time.Second

// RefuseLocal refuses an address that names this device or its network outright,
// for connections through a proxy, which resolves names itself and would reach
// such an address from where it stands.
func RefuseLocal(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	name := strings.TrimSuffix(strings.ToLower(host), ".")
	ip := net.ParseIP(host)
	if name == "localhost" || strings.HasSuffix(name, ".localhost") || numericHost(name) ||
		(ip != nil && !publicAddress(ip)) {

		return fmt.Errorf("%w: %s", errPrivateAddress, host)
	}
	return nil
}

// numericHost returns whether a host reads as a number, as 127.1 and 2130706433
// do to some resolvers, though they aren't IP addresses as Go reads them.
func numericHost(host string) bool {
	last := host[strings.LastIndexByte(host, '.')+1:]
	if last == "" || net.ParseIP(host) != nil {
		return false
	}
	_, err := strconv.ParseUint(last, 0, 64)
	return err == nil
}

// nonPublicRanges are the ranges that Go's checks leave out but that don't lead
// to the public internet, as carrier-grade NAT, which tailnets use too, and the
// IPv6 prefixes that wrap an IPv4 address, which could be a private one.
var nonPublicRanges = mustParseCIDRs("0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24",
	"198.18.0.0/15", "240.0.0.0/4", "64:ff9b::/96", "64:ff9b:1::/48", "2001::/32",
	"2002::/16", "fec0::/10")

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
// first of the URIs that gives it.  If none does, but a host gave another file,
// it returns that file with ErrRegistryChanged: a collection that's still being
// minted publishes the hash of the whole set, and serves the part minted so far.
// A gateway only ever has a file to lose, so another file from one doesn't count,
// even after a redirect, and neither does what isn't JSON, such as a host's page.
func (f *Fetcher) Fetch(ctx context.Context, hash [32]byte, uris []string) ([]byte, error) {
	if data := f.cached(hash); data != nil {
		return data, nil
	}
	var last error
	var changed []byte
	tries := 0
	for _, uri := range uris {
		for _, location := range f.locations(uri) {
			if tries++; tries > maxLocations {
				break
			}
			data, final, err := f.download(ctx, location, maxRegistrySize)
			if err == nil && sha256.Sum256(data) != hash {
				err = fmt.Errorf("%s doesn't match the published hash", location)
				if changed == nil && !strings.HasPrefix(uri, "ipfs://") &&
					ipfsPath(final) == "" && json.Valid(data) {

					changed = data
				}
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
	if changed != nil {
		return changed, ErrRegistryChanged
	}
	if last != nil {
		return nil, fmt.Errorf("%w: %v", ErrRegistryUnavailable, last)
	}
	return nil, ErrRegistryUnavailable
}

// locations returns the URLs to try for a URI, by the CHIP: no scheme means
// https, a domain alone means its well-known path, and ipfs goes through the
// gateways.  Any other scheme gives none.  A URL with an ipfs path is on a
// gateway of the publisher's choice, which can close, so the wallet's gateways
// follow it.
func (f *Fetcher) locations(uri string) []string {
	if len(uri) > maxURILen || !displayable(uri, maxURILen) {
		return nil
	}
	if rest, ok := strings.CutPrefix(uri, "ipfs://"); ok {
		if rest == "" {
			return nil
		}
		return f.gatewayLocations(rest)
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
	locations := []string{parsed.String()}
	if path := ipfsPath(parsed); path != "" {
		locations = append(locations, f.gatewayLocations(path)...)
	}
	return locations
}

// ipfsPath returns the ipfs path a URL on a gateway serves, as
// host/ipfs/<cid>/file or <cid>.ipfs.host/file have it, or else nothing.
func ipfsPath(location *url.URL) string {
	if path, ok := strings.CutPrefix(location.Path, "/ipfs/"); ok {
		return path
	}
	if cid, _, ok := strings.Cut(strings.ToLower(location.Hostname()), ".ipfs."); ok && cid != "" {
		return cid + location.Path
	}
	return ""
}

// gatewayLocations returns the URLs of an ipfs path on the gateways.
func (f *Fetcher) gatewayLocations(path string) []string {
	locations := make([]string, 0, len(f.Gateways))
	for _, gateway := range f.Gateways {
		locations = append(locations, strings.TrimRight(gateway, "/")+"/ipfs/"+path)
	}
	return locations
}

// download reads a URL, up to limit bytes, and returns the file and where it came
// from after any redirects.  It has no limit on its whole time, as a slow mobile
// link that keeps moving has to get there: it ends if it can't connect, by the
// transport's limits, or if it stalls, by watchRate.
func (f *Fetcher) download(ctx context.Context, location string, limit int64) ([]byte, *url.URL, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("%s: %s", location, resp.Status)
	}
	body := &countingReader{reader: resp.Body}
	stopWatching := watchRate(ctx, body, cancel, rateWindow)
	defer stopWatching()
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > limit {
		return nil, nil, fmt.Errorf("%s is over %d bytes", location, limit)
	}
	return data, resp.Request.URL, nil
}

// countingReader counts what's read through it.
type countingReader struct {
	reader io.Reader
	read   atomic.Int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read.Add(int64(n))
	return n, err
}

// watchRate ends a download that's slower than minDownloadRate, so that a host that
// stalls or barely drips a file doesn't use up the time of a download that other
// tokens wait behind.  A slow connection is far faster than that.  A host that keeps
// to the rate still has the download's whole time.
func watchRate(ctx context.Context, body *countingReader, cancel func(), window time.Duration) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(window)
		defer ticker.Stop()
		check := rateCheck{least: int64(window/time.Second) * minDownloadRate}
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if check.tooSlow(body.read.Load()) {
					cancel()
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

// rateCheck tells a download that read less than least in two windows in a row, as
// a healthy one can stall for a moment, when the phone changes networks.
type rateCheck struct {
	least int64
	last  int64
	slow  bool
}

// tooSlow takes what was read so far, at the end of a window.
func (c *rateCheck) tooSlow(read int64) bool {
	slow := read-c.last < c.least
	again := slow && c.slow
	c.last, c.slow = read, slow
	return again
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
	f.keepMu.Lock()
	defer f.keepMu.Unlock()
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
