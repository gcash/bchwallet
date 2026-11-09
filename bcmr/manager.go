package bcmr

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gcash/bchd/chaincfg/chainhash"
)

const (
	// cycleInterval is how often new blocks are looked at.
	cycleInterval = 30 * time.Second

	// maxRetryInterval is the longest a registry that keeps failing waits.
	maxRetryInterval = 24 * time.Hour

	// registryParser numbers what the wallet reads from a registry.  Raising it
	// has the registries read again, as what was kept from them can't show
	// what the parser has learned since.
	registryParser = 4

	// changedRegistryInterval is how often a registry that isn't the published
	// one is read again, as a collection still being minted adds to it.
	changedRegistryInterval = 24 * time.Hour
)

// TokenInfo is what the wallet can say of a token.
type TokenInfo struct {
	Category chainhash.Hash

	// Metadata is what to show, or nil if there's nothing yet.  It comes
	// from the token's registry, or else from the Known list.
	Metadata *Metadata

	Status Status

	// Known reports that the token is on the Known list.
	Known bool

	// InHints reports that the token loads by itself.  Any other token
	// needs Refresh.
	InHints bool

	// Stale reports that the publication shown isn't on the head, so a newer
	// head has published nothing.
	Stale bool

	// LookAlike reports that the token has the name or symbol of a Known
	// token, under another category, or a reserved symbol.
	LookAlike bool

	// ReservedSymbol reports that a LookAlike is one for its reserved symbol,
	// and ReservedName for its name, which is the coin's own.
	ReservedSymbol bool
	ReservedName   bool

	Burned bool

	// RegistryChanged reports that Metadata is from a file that isn't the
	// published one, so nothing vouches for it.
	RegistryChanged bool

	// RegistryFailed reports that the token published a registry, but there's
	// nothing to show from it.
	RegistryFailed bool
}

// Config is what a Manager needs.
type Config struct {
	Backend Backend
	Store   *Store
	Fetcher *Fetcher

	// Known is the Known list.
	Known map[chainhash.Hash]*Metadata

	// AutoLoad returns whether a token loads by itself: one in the hints
	// table.
	AutoLoad func(chainhash.Hash) bool

	// Ready returns whether the chain is ready to be asked.
	Ready func() bool

	// Tip returns the hash of the tip, so that a cycle with no new block and
	// nothing new to do costs nothing.
	Tip func() (*chainhash.Hash, error)

	// FilterRollbacks gives the height the filter headers were rolled back
	// to each time they were found wrong above it, for a backend that reads
	// filters to scan those blocks again, and StopRollbacks ends that.
	FilterRollbacks <-chan uint32
	StopRollbacks   func()
}

// retryKey is a token's registry, as tokens can publish the same one and fail on
// it apart.
type retryKey struct {
	category chainhash.Hash
	hash     [32]byte
}

// retry is a registry that couldn't be read.
type retry struct {
	last  time.Time
	tries uint
}

// registryRead is the read of a token's registry, and what it found.
type registryRead struct {
	state *State
	pub   *Publication

	// same is whether the state already has this publication.
	same bool

	// user is whether the user asked for the token to be read again.
	user bool

	data []byte
	err  error
}

// Manager keeps the metadata of the tokens the app asks about.  The app asks
// for the tokens it shows, so hidden ones cost nothing.
type Manager struct {
	cfg Config

	// mu guards views, wanted and asked.
	mu     sync.Mutex
	views  map[chainhash.Hash]TokenInfo
	wanted map[chainhash.Hash]bool // true if asked for by Refresh.

	// asked holds the tokens the app asked about since the start, the only
	// ones followed: a token it stopped showing, because it was hidden or
	// spent, costs nothing.
	asked map[chainhash.Hash]struct{}

	// pubURIs are the URIs of each token's current publication, for a
	// Refresh to tell whether any of them can be read now.
	pubURIs map[chainhash.Hash][]string

	// known are the Known tokens' names, by their skeletons, and symbols, as
	// every view is checked against them.
	known []knownName

	// The rest belongs to the run goroutine.
	states   map[chainhash.Hash]*State
	tried    map[retryKey]retry
	lastTip  chainhash.Hash
	lastFull time.Time

	// reread holds the tokens whose registry Refresh has read again, which
	// keep what shows until then.
	reread map[chainhash.Hash]struct{}

	// lastAsked is how many tokens had been asked about at the last cycle.
	lastAsked int

	// flights are the registry downloads under way, by the published hash
	// and the URIs, each with the reads that wait for it, reading the
	// categories with a read among them, and results where each download's
	// read comes back to the run goroutine.
	flights map[flightKey][]*registryRead
	reading map[chainhash.Hash]struct{}
	results chan *registryRead

	wake   chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
}

// NewManager returns a Manager with the states of the store.
func NewManager(cfg Config) (*Manager, error) {
	states, err := cfg.Store.All()
	if err != nil {
		return nil, err
	}
	m := &Manager{
		known:   knownNames(cfg.Known),
		cfg:     cfg,
		views:   make(map[chainhash.Hash]TokenInfo),
		wanted:  make(map[chainhash.Hash]bool),
		states:  states,
		asked:   make(map[chainhash.Hash]struct{}),
		pubURIs: make(map[chainhash.Hash][]string),
		tried:   make(map[retryKey]retry),
		reread:  make(map[chainhash.Hash]struct{}),
		wake:    make(chan struct{}, 1),
		flights: make(map[flightKey][]*registryRead),
		reading: make(map[chainhash.Hash]struct{}),
		results: make(chan *registryRead),
	}
	for _, state := range states {
		m.views[state.Category] = m.viewOf(state)
		if pub := state.CurrentPublication(); pub != nil {
			m.pubURIs[state.Category] = pub.URIs
		}
		if state.RetryTries > 0 {
			m.tried[retryKey{state.Category, state.RetryHash}] = retry{state.RetryLast, state.RetryTries}
		}
	}
	return m, nil
}

// knownName is what a Known token's name and symbol are checked against.
type knownName struct {
	name   string
	symbol string
}

// knownNames returns the names and symbols of the Known list to check against.
func knownNames(known map[chainhash.Hash]*Metadata) []knownName {
	names := make([]knownName, 0, len(known))
	for _, metadata := range known {
		names = append(names, knownName{skeleton(metadata.Name), baseSymbol(metadata.Symbol)})
	}
	return names
}

// Start begins following tokens.
func (m *Manager) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel, m.done = cancel, make(chan struct{})
	go m.run(ctx)
}

// Stop ends it, and waits for it.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
		<-m.done
	}
	if m.cfg.StopRollbacks != nil {
		m.cfg.StopRollbacks()
	}
}

// Tokens returns what's known of the categories, and starts on those that load
// by themselves.
func (m *Manager) Tokens(categories []chainhash.Hash) []TokenInfo {
	infos := make([]TokenInfo, 0, len(categories))
	wake := false
	m.mu.Lock()
	for _, category := range categories {
		info, tracked := m.views[category]
		if !tracked {
			info = m.viewOf(&State{Category: category})
		}
		_, asked := m.asked[category]
		// With nothing loading by itself, a token stored earlier waits to be
		// asked for too, as following it would reach the host it publishes.
		if (info.InHints || (tracked && m.cfg.AutoLoad != nil)) && !asked {
			m.asked[category] = struct{}{}
			wake = true
			if !tracked {
				m.wanted[category] = false
			}
		}
		infos = append(infos, info)
	}
	m.mu.Unlock()
	if wake {
		m.signal()
	}
	return infos
}

// Peek returns what's known of the categories, without having any of them load
// or be followed, as for tokens the wallet no longer holds.
func (m *Manager) Peek(categories []chainhash.Hash) []TokenInfo {
	infos := make([]TokenInfo, 0, len(categories))
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, category := range categories {
		info, tracked := m.views[category]
		if !tracked {
			info = m.viewOf(&State{Category: category})
		}
		infos = append(infos, info)
	}
	return infos
}

// Refresh fetches a token's metadata again, or for the first time, whether it
// loads by itself or not.  It returns ErrNoGateway if every location of the
// token's registry is paused or out of rotation, so that the user is told at
// once that it won't be read now, though it will be later.
func (m *Manager) Refresh(category chainhash.Hash) error {
	m.mu.Lock()
	m.wanted[category] = true
	m.asked[category] = struct{}{}
	uris := m.pubURIs[category]
	m.mu.Unlock()
	m.signal()
	if len(uris) > 0 && m.cfg.Fetcher != nil && m.cfg.Fetcher.Unavailable(uris) {
		return ErrNoGateway
	}
	return nil
}

func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) run(ctx context.Context) {
	defer close(m.done)
	ticker := time.NewTicker(cycleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case read := <-m.results:
			m.finishRead(ctx, read)
			continue
		case height := <-m.cfg.FilterRollbacks:
			m.rolledBack(int32(height))
		case <-m.wake:
		case <-ticker.C:
		}
		if err := m.cycle(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Warnf("Unable to follow the wallet's tokens: %v", err)
		}
	}
}

// cycle takes on what was asked for, starts reading what's published as soon
// as it's found, then follows the identities to the tip.  A Status is only left
// by a cycle that reaches its end.  It doesn't wait for registries, which come
// in as their downloads end, so that a slow host holds up no chain.
func (m *Manager) cycle(ctx context.Context) error {
	if m.cfg.Ready != nil && !m.cfg.Ready() {
		return nil
	}
	news := m.takeWanted()
	states := m.askedStates()
	if len(states) == 0 {
		return nil
	}

	// With nothing new and no new block, only what's due to be tried again
	// has anything to do.
	var tip chainhash.Hash
	if m.cfg.Tip != nil {
		hash, err := m.cfg.Tip()
		if err != nil {
			return err
		}
		tip = *hash
		if !news && tip == m.lastTip && time.Since(m.lastFull) < retryInterval {
			return nil
		}
	}

	if err := m.cfg.Backend.Prepare(ctx, states); err != nil {
		return err
	}
	for _, state := range states {
		m.save(state)
	}
	m.startReads(ctx, states)

	// A read that comes back during a long scan shows at its next save.
	save := func(state *State) {
		m.save(state)
		m.takeReads(ctx)
	}
	if err := m.cfg.Backend.Advance(ctx, states, save); err != nil {
		return err
	}
	m.startReads(ctx, states)
	for _, state := range states {
		m.save(state)
	}
	m.lastTip, m.lastFull = tip, time.Now()
	return nil
}

// askedStates returns the states of the tokens the app asked about.
func (m *Manager) askedStates() []*State {
	m.mu.Lock()
	defer m.mu.Unlock()
	states := make([]*State, 0, len(m.asked))
	for category := range m.asked {
		if state, ok := m.states[category]; ok {
			states = append(states, state)
		}
	}
	return states
}

// takeWanted turns what was asked for into states.  A Refresh has the registry
// read again, and forgets a failure, so that it's tried again.
// It returns whether there was anything to take.
func (m *Manager) takeWanted() bool {
	m.mu.Lock()
	wanted := m.wanted
	m.wanted = make(map[chainhash.Hash]bool)
	asked := len(m.asked) != m.lastAsked
	m.lastAsked = len(m.asked)
	m.mu.Unlock()
	for category, refresh := range wanted {
		state, ok := m.states[category]
		if !ok {
			state = &State{Category: category}
			m.states[category] = state
			m.save(state)
		}
		if refresh {
			m.reread[category] = struct{}{}
			if backend, ok := m.cfg.Backend.(interface{ Forget(chainhash.Hash) }); ok {
				backend.Forget(category)
			}
			state.RegistryFailed = false
			if state.Status == StatusFailed || state.Status == StatusUnavailable {
				state.Status = StatusSearching
			}
			if pub := state.CurrentPublication(); pub != nil {
				delete(m.tried, retryKey{category, pub.Hash})
			}
			state.RetryHash, state.RetryTries, state.RetryLast = [32]byte{}, 0, time.Time{}
		}
	}
	return asked || len(wanted) != 0
}

// flightKey is a download: a registry's hash and the URIs it's read from.
type flightKey struct {
	hash [32]byte
	uris string
}

// keyOf returns the download of a read.
func keyOf(read *registryRead) flightKey {
	return flightKey{read.pub.Hash, strings.Join(read.pub.URIs, "\x00")}
}

// startReads starts reading the registry of each state whose publication
// changed, unless one is under way.  The Fetcher limits the downloads at each
// host.  Each read comes back to the
// run goroutine, which the states and what's tried belong to, as finishRead.
// Tokens can share a registry, so a read joins a download under way of the
// same file from the same URIs.  One from other URIs reads them, as a stranger
// can publish a token's hash with URIs that keep it waiting, and the Fetcher
// has the first file that matches the hash serve both.
func (m *Manager) startReads(ctx context.Context, states []*State) {
	for _, state := range states {
		if ctx.Err() != nil {
			return
		}
		if _, ok := m.reading[state.Category]; ok {
			continue
		}
		read := m.registryDue(state)
		if read == nil {
			continue
		}
		key := keyOf(read)
		if sharers, ok := m.flights[key]; ok {
			m.flights[key] = append(sharers, read)
			m.reading[state.Category] = struct{}{}
			continue
		}
		m.flights[key] = []*registryRead{read}
		m.reading[state.Category] = struct{}{}

		// A token the user refreshed goes ahead of those that load by
		// themselves.
		fetchCtx := ctx
		if read.user {
			fetchCtx = UserAction(ctx)
		}
		go func() {
			read.data, read.err = m.cfg.Fetcher.Fetch(fetchCtx, read.pub.Hash, read.pub.URIs)
			select {
			case m.results <- read:
			case <-ctx.Done():
			}
		}()
	}
}

// rolledBack has the backend set back each state's scans above the height the
// filter headers were rolled back to, and the next cycle scan them again.
func (m *Manager) rolledBack(height int32) {
	backend, ok := m.cfg.Backend.(interface {
		RolledBack(*State, int32) (bool, error)
	})
	if !ok {
		return
	}
	log.Infof("Scanning the tokens' blocks above %d again, as their filters were wrong", height)
	for _, state := range m.states {
		changed, err := backend.RolledBack(state, height)
		if err != nil {
			log.Warnf("Unable to scan token %v again above %d: %v", state.Category, height, err)
			continue
		}
		if changed {
			m.save(state)
		}
	}
	m.lastFull = time.Time{}
}

// takeReads keeps the registry reads that came back, without waiting for the
// others.
func (m *Manager) takeReads(ctx context.Context) {
	for {
		select {
		case read := <-m.results:
			m.finishRead(ctx, read)
		default:
			return
		}
	}
}

// finishRead keeps what a download found for the reads that waited for it,
// and starts the reads that were left waiting for room.  A read whose token
// published again meanwhile counts for nothing.
func (m *Manager) finishRead(ctx context.Context, read *registryRead) {
	key := keyOf(read)
	reads := m.flights[key]
	delete(m.flights, key)
	for _, waiting := range reads {
		delete(m.reading, waiting.state.Category)
		waiting.data, waiting.err = read.data, read.err
		if pub := waiting.state.CurrentPublication(); pub == nil || pub.Hash != waiting.pub.Hash ||
			!slices.Equal(pub.URIs, waiting.pub.URIs) {

			continue
		}
		m.useRegistry(ctx, waiting)
	}
	m.startReads(ctx, m.askedStates())
}

// registryDue returns the read a state's registry needs, if any.
func (m *Manager) registryDue(state *State) *registryRead {
	pub := state.CurrentPublication()
	if pub == nil {
		if state.Metadata != nil || state.RegistryHash != [32]byte{} || state.RegistryFailed {
			state.clearRegistry()
			m.save(state)
		}
		return nil
	}
	// A hash of zeros is what nothing read yet has too, so it takes what
	// was read to tell them apart.
	read := state.RegistryHash != [32]byte{} || state.Metadata != nil || state.RegistryFailed
	same := pub.Hash == state.RegistryHash && read
	_, reread := m.reread[state.Category]
	// A file that isn't the published one is read daily, whatever snapshot it
	// claims is due, as its host could otherwise have it read at every block.
	due := snapshotDue(state) && !state.RegistryChanged
	if same && !reread && state.RegistryParser == registryParser && !due &&
		(!state.RegistryChanged || time.Since(state.RegistryRead) < changedRegistryInterval) {

		// A state from before failures were kept has nothing to show.
		if state.Metadata == nil && !state.RegistryFailed {
			state.RegistryFailed = true
			m.save(state)
		}
		return nil
	}

	// What showed was from another publication, which this one
	// replaced, so it no longer counts.
	if !same && state.Metadata != nil {
		state.clearRegistry()
		m.save(state)
	}
	key := retryKey{state.Category, pub.Hash}
	if failed, ok := m.tried[key]; ok && time.Since(failed.last) < retryWait(failed.tries) {
		return nil
	}
	return &registryRead{state: state, pub: pub, same: same, user: reread}
}

// useRegistry keeps what a read found.
func (m *Manager) useRegistry(ctx context.Context, read *registryRead) {
	if ctx.Err() != nil {
		return
	}
	state, pub, same, data, err := read.state, read.pub, read.same, read.data, read.err
	key := retryKey{state.Category, pub.Hash}
	changed := errors.Is(err, ErrRegistryChanged)
	var metadata *Metadata
	if err == nil || changed {
		var parseErr error
		metadata, parseErr = ParseRegistry(data, state.Category, time.Now())
		if parseErr != nil {
			log.Debugf("Registry of token %v: %v", state.Category, parseErr)
			metadata = nil
		}
	}

	// A file that can't be used on a second reading loses nothing that
	// shows, as a parser that changed could be at fault.
	if err == nil && metadata == nil && same && state.Metadata != nil {
		err = ErrNoMetadata
	}

	// Only a collection adds to its registry as it goes, so a file that
	// isn't the published one counts for nothing else.  Its decimals
	// don't count either, as they would change how amounts read.
	if changed {
		switch {
		case metadata == nil || len(metadata.NFTTypes) == 0:
			err, metadata = fmt.Errorf("%w, for a token that isn't a collection", err), nil

		// What was read of the published file stays, when reading it
		// again gets another.
		case same && state.Metadata != nil && !state.RegistryChanged:
			metadata = nil

		default:
			err = nil
			metadata.Decimals = 0
		}
	}
	// A Refresh's read is over, so later ones are the token's own.
	delete(m.reread, state.Category)
	if err != nil {
		failed := m.tried[key]
		failed = retry{time.Now(), failed.tries + 1}
		m.tried[key] = failed
		log.Debugf("Unable to read the registry of token %v: %v", state.Category, err)
		state.RetryHash, state.RetryTries, state.RetryLast = pub.Hash, failed.tries, failed.last
		if state.Metadata == nil {
			state.RegistryFailed = true
		}
		m.save(state)
		return
	}
	delete(m.tried, key)
	state.RetryHash, state.RetryTries, state.RetryLast = [32]byte{}, 0, time.Time{}
	state.RegistryHash, state.Metadata = pub.Hash, metadata
	state.RegistryChanged, state.RegistryFailed = changed, metadata == nil
	state.RegistryParser = registryParser
	state.RegistryRead = time.Time{}
	if changed {
		state.RegistryRead = time.Now()
	}
	m.save(state)
}

// snapshotDue returns whether a snapshot that was still to come has taken over.
func snapshotDue(state *State) bool {
	return state.Metadata != nil && !state.Metadata.NextSnapshot.IsZero() &&
		!time.Now().Before(state.Metadata.NextSnapshot)
}

// retryWait returns how long a registry waits after failing tries times.
func retryWait(tries uint) time.Duration {
	if tries > 8 {
		return maxRetryInterval
	}
	return min(retryInterval<<(tries-1), maxRetryInterval)
}

// save stores a state and updates what the app sees.
func (m *Manager) save(state *State) {
	if err := m.cfg.Store.Put(state); err != nil {
		log.Warnf("Unable to save token %v: %v", state.Category, err)
	}
	view := m.viewOf(state)
	pub := state.CurrentPublication()
	m.mu.Lock()
	m.views[state.Category] = view
	if pub != nil {
		m.pubURIs[state.Category] = pub.URIs
	} else {
		delete(m.pubURIs, state.Category)
	}
	m.mu.Unlock()
}

// viewOf returns what the app sees of a state.
func (m *Manager) viewOf(state *State) TokenInfo {
	info := TokenInfo{
		Category: state.Category,
		Metadata: state.Metadata,
		Status:   state.Status,
		Burned:   state.Burned,
		Stale:    state.Metadata != nil && state.PublicationStale(),

		RegistryChanged: state.RegistryChanged,
		RegistryFailed:  state.RegistryFailed,
	}
	if m.cfg.AutoLoad != nil {
		info.InHints = m.cfg.AutoLoad(state.Category)
	}
	if known, ok := m.cfg.Known[state.Category]; ok {
		info.Known = true
		if info.Metadata == nil || info.RegistryChanged {
			info.Metadata = known
		}
		info.RegistryChanged, info.RegistryFailed = false, false
	}
	if !info.Known && info.Metadata != nil {
		name := skeleton(info.Metadata.Name)
		info.ReservedSymbol = reservedSymbols[baseSymbol(info.Metadata.Symbol)]
		info.ReservedName = reservedNames[name]
		info.LookAlike = info.ReservedSymbol || info.ReservedName || m.looksKnown(name, info.Metadata.Symbol)
	}
	return info
}

// looksKnown returns whether a name, by its skeleton, copies a Known token's, or
// a symbol is one.
func (m *Manager) looksKnown(name, symbol string) bool {
	symbol = baseSymbol(symbol)
	for _, known := range m.known {
		if nameCopies(name, known.name) || known.symbol == symbol {
			return true
		}
	}
	return false
}

// baseSymbol returns a symbol up to its first hyphen, which is what the CHIP has
// be unique.
func baseSymbol(symbol string) string {
	base, _, _ := strings.Cut(strings.ToUpper(symbol), "-")
	return base
}
