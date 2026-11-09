package bcmr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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

	// maxRounds is how many times a fetch goes round an item's locations,
	// roundPause apart, before it fails, as a gateway that's slow to find a
	// file keeps looking after the wallet hangs up, and often has it later.
	maxRounds  = 3
	roundPause = 30 * time.Second

	// minDownloadRate is the slowest a download can be, in bytes a second over
	// rateWindow.
	minDownloadRate = 1024

	// maxPerHost is how many registries, and apart from them how many icons,
	// download at once from one host, so that a gateway isn't flooded and
	// neither kind waits behind the other.
	maxPerHost = 4

	// gatewayRestFirst and gatewayRestMost are how long a gateway that failed
	// is out of rotation: the first, doubling with each failure in a row, up to
	// the most.  It's back afterwards, as a gateway that fails now is often
	// fine later.
	gatewayRestFirst = 30 * time.Second
	gatewayRestMost  = 5 * time.Minute

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

	// errGatewayFault is a failure that's the host's: one that doesn't connect,
	// answers with a 5xx, or stalls.
	errGatewayFault = errors.New("the host failed")

	// ErrNoGateway is the answer to an icon or a Refresh the user asked for
	// while every gateway or host that has it is paused or out of rotation,
	// rather than have the user wait with no end in sight.
	ErrNoGateway = errors.New("every gateway is paused or out of rotation")

	// errRateLimited is a host that answered 429, Too Many Requests.
	errRateLimited = errors.New("the host is rate limiting")

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

	// registries and icons are the slots at each host for each kind.
	registries *hostSlots
	icons      *hostSlots

	// itemsMu guards items, the requests being downloaded, by what they are.
	itemsMu sync.Mutex
	items   map[string]*sharedItem

	// now is the time, apart for tests.
	now func() time.Time

	// roundPause is the wait between rounds, which tests leave out.
	roundPause time.Duration

	// healthMu guards out, the gateways out of rotation, by host.  Only memory
	// keeps them, so they start afresh with the wallet.
	healthMu sync.Mutex
	out      map[string]gatewayOut

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
	f := newFetcher(gateways, cacheDir, &http.Transport{
		DialContext:           dial,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		DisableKeepAlives:     true,
	})
	f.roundPause = roundPause
	return f
}

func newFetcher(gateways []string, cacheDir string, transport http.RoundTripper) *Fetcher {
	f := &Fetcher{
		Gateways:   gateways,
		CacheDir:   cacheDir,
		registries: newHostSlots(maxPerHost),
		icons:      newHostSlots(maxPerHost),
		now:        time.Now,
		items:      make(map[string]*sharedItem),
		out:        make(map[string]gatewayOut),
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
	f.registries.usable = f.available
	f.icons.usable = f.available
	return f
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
	return f.shared(ctx, "registry "+hex.EncodeToString(hash[:]), func(ctx context.Context) ([]byte, error) {
		return f.fetch(ctx, hash, uris)
	})
}

// fetch is Fetch for one copy of the request.
func (f *Fetcher) fetch(ctx context.Context, hash [32]byte, uris []string) ([]byte, error) {
	return f.inRounds(ctx, func() ([]byte, bool, error) {
		return f.fetchRound(ctx, hash, uris)
	})
}

// inRounds runs round up to maxRounds times, roundPause apart, while it fails
// and says another round may do better, as mayPass has it of some location's
// failure.  A round that was given up on, or that had an answer, ends it.
func (f *Fetcher) inRounds(ctx context.Context, round func() ([]byte, bool, error)) ([]byte, error) {
	for n := 1; ; n++ {
		data, again, err := round()
		if err == nil || !again || n == maxRounds {
			return data, err
		}
		timer := time.NewTimer(f.roundPause)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}
}

// mayPass returns whether a location that failed may give the file later: not if
// it gave another file, or is an address that's always refused.
func mayPass(err error, wrongFile bool) bool {
	return !wrongFile && !errors.Is(err, errPrivateAddress)
}

// fetchRound is one round of fetch, and returns whether another may do better.
// A location whose hosts are all out of rotation is passed over for the next,
// and only when every location is out does the round wait for one to come back.
func (f *Fetcher) fetchRound(ctx context.Context, hash [32]byte, uris []string) ([]byte, bool, error) {
	for {
		data, again, out, err := f.fetchRoundOnce(ctx, hash, uris)
		if out == nil {
			return data, again, err
		}
		if err := f.waitBack(ctx, f.registries, hostsOf(out)); err != nil {
			return nil, false, err
		}
	}
}

// fetchRoundOnce is a pass of fetchRound over the locations in rotation, and
// returns the locations out of it if they were all there was.
func (f *Fetcher) fetchRoundOnce(ctx context.Context, hash [32]byte, uris []string) ([]byte, bool, []string, error) {
	if data := f.cached(hash); data != nil {
		return data, false, nil, nil
	}
	var out []string
	var last error
	var again bool
	var changed []byte
	tries := 0
	tried := make(map[string]bool)
uris:
	for _, uri := range uris {
		for _, group := range f.locationGroups(uri) {
			// Each gateway in turn, the first with room first, until one has it.
			// A location already tried, as a publisher's URL on a gateway is,
			// isn't tried again, nor waited for.
			left := untried(group, tried)
			if skipped := f.outOfRotation(left); skipped != nil {
				out = append(out, skipped...)
				continue
			}
			for len(left) > 0 {
				if tries++; tries > maxLocations {
					break uris
				}
				data, final, location, err := f.downloadAny(ctx, f.registries, left, maxRegistrySize)
				if location == "" {
					return nil, false, nil, err
				}
				tried[location] = true
				left = untried(left, tried)
				wrongFile := err == nil && sha256.Sum256(data) != hash
				if wrongFile {
					err = fmt.Errorf("%s doesn't match the published hash", location)
					if failsItsCID(location, data) {
						f.failed(hostOf(location))
					}
					if changed == nil && !strings.HasPrefix(uri, "ipfs://") &&
						ipfsPath(final) == "" && json.Valid(data) {

						changed = data
					}
				}
				if err != nil {
					if ctx.Err() != nil {
						return nil, false, nil, ctx.Err()
					}
					last = err
					again = again || mayPass(err, wrongFile)
					continue
				}
				f.keep(hash, data)
				return data, false, nil, nil
			}
		}
	}
	if len(tried) == 0 && out != nil {
		return nil, false, out, ErrRegistryUnavailable
	}
	if changed != nil {
		return changed, false, nil, ErrRegistryChanged
	}
	if last != nil {
		return nil, again, nil, fmt.Errorf("%w: %v", ErrRegistryUnavailable, last)
	}
	return nil, false, nil, ErrRegistryUnavailable
}

// locations returns the URLs to try for a URI, by the CHIP: no scheme means
// https, a domain alone means its well-known path, and ipfs goes through the
// gateways.  Any other scheme gives none.  A URL with an ipfs path is on a
// gateway of the publisher's choice, which can close, so the wallet's gateways
// follow it.
func (f *Fetcher) locations(uri string) []string {
	var locations []string
	for _, group := range f.locationGroups(uri) {
		locations = append(locations, group...)
	}
	return locations
}

// locationGroups returns the locations of a URI as locations returns them, in
// groups that hold the same file on different hosts: the gateways' for a file on
// IPFS, and a publisher's own location alone.
func (f *Fetcher) locationGroups(uri string) [][]string {
	if len(uri) > maxURILen || !displayable(uri, maxURILen) {
		return nil
	}
	if rest, ok := strings.CutPrefix(uri, "ipfs://"); ok {
		if rest == "" {
			return nil
		}
		return f.gatewayGroup(rest)
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
	groups := [][]string{{parsed.String()}}
	if path := ipfsPath(parsed); path != "" {
		groups = append(groups, f.gatewayGroup(path)...)
	}
	return groups
}

// untried returns the locations not tried yet.
func untried(locations []string, tried map[string]bool) []string {
	var left []string
	for _, location := range locations {
		if !tried[location] {
			left = append(left, location)
		}
	}
	return left
}

// gatewayGroup returns the gateways' locations of an ipfs path as a group, or no
// group if there are no gateways.
func (f *Fetcher) gatewayGroup(path string) [][]string {
	if locations := f.gatewayLocations(path); len(locations) > 0 {
		return [][]string{locations}
	}
	return nil
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
func (f *Fetcher) download(ctx context.Context, slots *hostSlots, location string,
	limit int64) ([]byte, *url.URL, error) {

	data, final, _, err := f.downloadAny(ctx, slots, []string{location}, limit)
	return data, final, err
}

// downloadAny downloads the file at the first of the locations, which are the
// same file on different hosts, whose host has room, and returns the location
// it used, or "" if it gave up waiting.
func (f *Fetcher) downloadAny(ctx context.Context, slots *hostSlots, locations []string,
	limit int64) ([]byte, *url.URL, string, error) {

	// The wait for a slot isn't part of the download, which starts after it.
	hosts := make([]string, len(locations))
	for i, location := range locations {
		hosts[i] = hostOf(location)
	}
	// With every gateway paused or out, what loads by itself, and any registry,
	// waits for one to come back, while an icon the user asked for is told so.
	// A wait for a slot whose hosts all left rotation meanwhile starts over.
	var host string
	var release func()
	for {
		if err := f.waitBack(ctx, slots, hosts); err != nil {
			return nil, nil, "", err
		}
		in, _ := f.inRotation(hosts)
		if len(in) == 0 {
			continue
		}
		var err error
		host, release, err = slots.takeAny(ctx, f.avoidOthers(ctx, in))
		if errors.Is(err, errLeftRotation) {
			continue
		}
		if err != nil {
			return nil, nil, "", err
		}
		break
	}
	defer release()
	defer f.using(ctx, host)()
	location := locations[slices.Index(hosts, host)]
	data, final, err := f.get(ctx, location, limit)
	return data, final, location, err
}

// failsItsCID returns whether a file that isn't the published one isn't what the
// location's IPFS path names either, which shows the gateway at fault.  A file
// that the path names, or one at a path that can't be checked, as a directory's
// is, says nothing of the gateway: the publication named another file.
func failsItsCID(location string, data []byte) bool {
	parsed, err := url.Parse(location)
	if err != nil {
		return false
	}
	path := ipfsPath(parsed)
	if path == "" {
		return false
	}
	digest, raw := ipfsRawDigest("ipfs://" + path)
	return raw && sha256.Sum256(data) != digest
}

// waitBack waits for one of the hosts to be in rotation, as what loads by itself
// and any registry do, while an icon the user asked for is told that every one is
// out instead.
func (f *Fetcher) waitBack(ctx context.Context, slots *hostSlots, hosts []string) error {
	for {
		in, back := f.inRotation(hosts)
		if len(in) > 0 {
			return nil
		}
		if slots == f.icons && isUserAction(ctx) {
			return ErrNoGateway
		}
		timer := time.NewTimer(back)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}

// outOfRotation returns the locations whose hosts are all out of rotation, if
// they all are, or nil.
func (f *Fetcher) outOfRotation(locations []string) []string {
	if in, _ := f.inRotation(hostsOf(locations)); len(in) > 0 {
		return nil
	}
	return locations
}

// hostsOf returns the hosts of the locations.
func hostsOf(locations []string) []string {
	hosts := make([]string, len(locations))
	for i, location := range locations {
		hosts[i] = hostOf(location)
	}
	return hosts
}

// sharedItem is something being downloaded, by however many requests for it, and
// the file once one of them has it.
type sharedItem struct {
	copies int
	done   chan struct{}
	data   []byte

	// hosts are those its requests are downloading from, guarded by itemsMu.
	hosts map[string]int
}

// sharedItemKey is the context key of the item a request is for.
type sharedItemKey struct{}

// shared runs a request for an item, which other requests for the same item can be
// running too, as a user can ask again when a gateway is slow.  The first to get
// the file ends the others, which return it as well.
func (f *Fetcher) shared(ctx context.Context, key string, run func(context.Context) ([]byte, error)) ([]byte, error) {
	f.itemsMu.Lock()
	item, ok := f.items[key]
	if !ok {
		item = &sharedItem{done: make(chan struct{}), hosts: make(map[string]int)}
		f.items[key] = item
	}
	item.copies++
	f.itemsMu.Unlock()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-item.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	data, err := run(context.WithValue(ctx, sharedItemKey{}, item))

	f.itemsMu.Lock()
	defer f.itemsMu.Unlock()
	if err == nil && item.data == nil {
		item.data = data
		close(item.done)
	}
	if item.copies--; item.copies == 0 {
		delete(f.items, key)
	}
	if item.data != nil {
		return item.data, nil
	}
	return data, err
}

// avoidOthers returns the hosts but those that other requests for the same item
// are downloading from, so that asking again tries somewhere else, or all of them
// if that leaves none.
func (f *Fetcher) avoidOthers(ctx context.Context, hosts []string) []string {
	item, ok := ctx.Value(sharedItemKey{}).(*sharedItem)
	if !ok {
		return hosts
	}
	f.itemsMu.Lock()
	defer f.itemsMu.Unlock()
	var others []string
	for _, host := range hosts {
		if item.hosts[host] == 0 {
			others = append(others, host)
		}
	}
	if len(others) == 0 {
		return hosts
	}
	return others
}

// using records that a request for an item downloads from a host, and returns
// what records it's done.
func (f *Fetcher) using(ctx context.Context, host string) func() {
	item, ok := ctx.Value(sharedItemKey{}).(*sharedItem)
	if !ok {
		return func() {}
	}
	f.itemsMu.Lock()
	item.hosts[host]++
	f.itemsMu.Unlock()
	return func() {
		f.itemsMu.Lock()
		defer f.itemsMu.Unlock()
		if item.hosts[host]--; item.hosts[host] <= 0 {
			delete(item.hosts, host)
		}
	}
}

// gatewayOut is a gateway out of rotation: its failures in a row, and until when.
type gatewayOut struct {
	failures int
	until    time.Time
}

// inRotation returns the hosts but the gateways out of rotation, in their order,
// and if that leaves none, how long until the first is back.
func (f *Fetcher) inRotation(hosts []string) ([]string, time.Duration) {
	f.healthMu.Lock()
	defer f.healthMu.Unlock()
	now := f.now()
	var in []string
	var back time.Duration
	for _, host := range hosts {
		out, ok := f.out[host]
		if !ok || !now.Before(out.until) {
			in = append(in, host)
			continue
		}
		if wait := out.until.Sub(now); back == 0 || wait < back {
			back = wait
		}
	}
	return in, back
}

// Unavailable returns whether every location of the URIs is at a host that's
// paused or out of rotation, so that reading them now would only wait.  URIs
// with no location give no reason to wait.
func (f *Fetcher) Unavailable(uris []string) bool {
	var hosts []string
	for _, uri := range uris {
		for _, group := range f.locationGroups(uri) {
			for _, location := range group {
				hosts = append(hosts, hostOf(location))
			}
		}
	}
	if len(hosts) == 0 {
		return false
	}
	in, _ := f.inRotation(hosts)
	return len(in) == 0
}

// available returns whether a host is in rotation.
func (f *Fetcher) available(host string) bool {
	in, _ := f.inRotation([]string{host})
	return len(in) == 1
}

// leftRotation has the waits for slots whose hosts all left rotation go back to
// waiting for one to come back.  It's called with healthMu free, as the slots
// ask whether a host is in rotation while holding their own lock.
func (f *Fetcher) leftRotation() {
	f.registries.recheck()
	f.icons.recheck()
}

// failed takes a gateway that failed out of rotation, for longer the more times
// in a row it has.  A host that isn't a gateway has no rotation to leave.
func (f *Fetcher) failed(host string) {
	if !f.isGateway(host) {
		return
	}
	f.healthMu.Lock()
	out := f.out[host]
	out.failures++
	rest := gatewayRestMost
	if out.failures < 10 {
		rest = min(gatewayRestFirst<<(out.failures-1), gatewayRestMost)
	}
	out.until = f.now().Add(rest)
	f.out[host] = out
	f.healthMu.Unlock()
	f.leftRotation()
}

// worked puts a gateway back in rotation, with no failures counted.
func (f *Fetcher) worked(host string) {
	f.healthMu.Lock()
	defer f.healthMu.Unlock()
	delete(f.out, host)
}

// isGateway returns whether a host is one of the gateways.
func (f *Fetcher) isGateway(host string) bool {
	for _, gateway := range f.Gateways {
		if hostOf(gateway) == host {
			return true
		}
	}
	return false
}

// get reads a URL, up to limit bytes, and keeps count of a gateway's failures:
// not connecting, a 5xx, or a file that stalls.  A 404 only says that gateway
// doesn't have that file, and a caller that gave up says nothing of the host.
func (f *Fetcher) get(ctx context.Context, location string, limit int64) ([]byte, *url.URL, error) {
	host := hostOf(location)
	data, final, err := f.read(ctx, location, limit)
	switch {
	case err == nil:
		f.worked(host)
	case ctx.Err() != nil:
	case errors.Is(err, errGatewayFault):
		f.failed(host)
	case errors.Is(err, errRateLimited):
		var limit *rateLimit
		if errors.As(err, &limit) && limit.wait > 0 {
			f.pause(host, limit.wait)
		} else {
			f.failed(host)
		}
	}
	return data, final, err
}

// rateLimit is a 429 answer, and how long it asked us to wait, or 0 if it didn't.
type rateLimit struct {
	location string
	wait     time.Duration
}

func (l *rateLimit) Error() string {
	return fmt.Sprintf("%s: %v", l.location, errRateLimited)
}

func (l *rateLimit) Unwrap() error {
	return errRateLimited
}

// retryAfter returns how long a Retry-After header asks to wait, in seconds or
// until a date, or 0 for none or one that can't be read.
func retryAfter(header string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(header), 10, 64); err == nil {
		if seconds <= 0 || seconds > int64(math.MaxInt64/time.Second) {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(header); err == nil && when.After(now) {
		return when.Sub(now)
	}
	return 0
}

// pause takes a gateway that asked us to wait out of rotation for exactly as long
// as it asked, however long.
func (f *Fetcher) pause(host string, wait time.Duration) {
	if !f.isGateway(host) {
		return
	}
	f.healthMu.Lock()
	out := f.out[host]
	if until := f.now().Add(wait); until.After(out.until) {
		out.until = until
	}
	f.out[host] = out
	f.healthMu.Unlock()
	f.leftRotation()
}

// read reads a URL, up to limit bytes, and wraps errGatewayFault in what's the
// host's fault.
func (f *Fetcher) read(ctx context.Context, location string, limit int64) ([]byte, *url.URL, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", errGatewayFault, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError {
		return nil, nil, fmt.Errorf("%w: %s: %s", errGatewayFault, location, resp.Status)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, nil, &rateLimit{location, retryAfter(resp.Header.Get("Retry-After"), f.now())}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("%s: %s", location, resp.Status)
	}
	body := &countingReader{reader: resp.Body}
	stopWatching := watchRate(ctx, body, cancel, rateWindow)
	defer stopWatching()
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	// A download ended for being too slow has failed, even if the file ended
	// first, as a host that stops sending can close it cleanly: what came is
	// only part of it.
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", errGatewayFault, err)
	}
	if int64(len(data)) > limit {
		return nil, nil, fmt.Errorf("%s is over %d bytes", location, limit)
	}
	return data, resp.Request.URL, nil
}

// hostOf returns the host of a URL, or nothing for one that can't be read.
func hostOf(location string) string {
	parsed, err := url.Parse(location)
	if err != nil {
		return ""
	}
	return parsed.Host
}

// hostSlots lets so many downloads at once at each host, and has the others wait
// in one line, in the order they came.  A download that can use any of several
// hosts, as a file on IPFS can through any gateway, takes the first of them that
// has room, and in line, the first of them that comes free.  A slot that comes
// free at a host out of rotation, which usable tells, isn't handed on, and a
// wait for hosts that all left rotation ends with errLeftRotation.
type hostSlots struct {
	limit   int
	usable  func(host string) bool
	mu      sync.Mutex
	busy    map[string]int
	waiting []*slotWaiter
}

// errLeftRotation ends a wait for a slot whose hosts all left rotation.
var errLeftRotation = errors.New("every host of the wait left rotation")

// slotWaiter is a download in line, and the hosts it can use.
type slotWaiter struct {
	hosts []string
	turn  chan string
	user  bool
}

// userActionKey is the context key that marks what the user asked for.
type userActionKey struct{}

// UserAction marks a request as asked for by the user, which goes ahead of all
// that loads by itself in the lines for hosts.
func UserAction(ctx context.Context) context.Context {
	return context.WithValue(ctx, userActionKey{}, true)
}

// isUserAction returns whether a request was asked for by the user.
func isUserAction(ctx context.Context) bool {
	user, _ := ctx.Value(userActionKey{}).(bool)
	return user
}

func newHostSlots(limit int) *hostSlots {
	return &hostSlots{limit: limit, busy: make(map[string]int)}
}

// take waits for a slot at the host, and returns what gives it back.
func (s *hostSlots) take(ctx context.Context, host string) (func(), error) {
	_, release, err := s.takeAny(ctx, []string{host})
	return release, err
}

// takeAny waits for a slot at the first of the hosts that has one, in their order,
// and returns the host and what gives the slot back.
func (s *hostSlots) takeAny(ctx context.Context, hosts []string) (string, func(), error) {
	s.mu.Lock()
	for _, host := range hosts {
		if s.busy[host] < s.limit {
			s.busy[host]++
			s.mu.Unlock()
			return host, s.giver(host), nil
		}
	}
	w := &slotWaiter{hosts: hosts, turn: make(chan string, 1), user: isUserAction(ctx)}
	if w.user {
		// After the users ahead, before everything else.
		at := 0
		for at < len(s.waiting) && s.waiting[at].user {
			at++
		}
		s.waiting = slices.Insert(s.waiting, at, w)
	} else {
		s.waiting = append(s.waiting, w)
	}
	s.mu.Unlock()

	select {
	case host := <-w.turn:
		if host == "" {
			return "", nil, errLeftRotation
		}
		return host, s.giver(host), nil
	case <-ctx.Done():
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, waiting := range s.waiting {
			if waiting == w {
				s.waiting = append(s.waiting[:i], s.waiting[i+1:]...)
				return "", nil, ctx.Err()
			}
		}
		// A slot came as the wait ended, so it goes to the next in line.
		if host := <-w.turn; host != "" {
			s.handOn(host)
		}
		return "", nil, ctx.Err()
	}
}

// recheck ends the waits whose hosts all left rotation, which go back to
// waiting for one to come back.
func (s *hostSlots) recheck() {
	if s.usable == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.waiting[:0]
	for _, w := range s.waiting {
		if slices.ContainsFunc(w.hosts, s.usable) {
			kept = append(kept, w)
			continue
		}
		w.turn <- ""
	}
	clear(s.waiting[len(kept):])
	s.waiting = kept
}

// giver returns what gives a slot at the host back.
func (s *hostSlots) giver(host string) func() {
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.handOn(host)
	}
}

// handOn passes a slot at the host that's free to the first in line that can use
// the host, if it's in rotation, or frees it.
func (s *hostSlots) handOn(host string) {
	if s.usable == nil || s.usable(host) {
		for i, w := range s.waiting {
			if slices.Contains(w.hosts, host) {
				s.waiting = append(s.waiting[:i], s.waiting[i+1:]...)
				w.turn <- host
				return
			}
		}
	}
	if s.busy[host]--; s.busy[host] <= 0 {
		delete(s.busy, host)
	}
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
