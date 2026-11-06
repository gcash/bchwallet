package bcmr

import (
	"context"
	"errors"
	"fmt"
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
	// token, under another category.
	LookAlike bool

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
}

// retry is a registry that couldn't be read.
type retry struct {
	last  time.Time
	tries uint
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

	// The rest belongs to the run goroutine.
	states   map[chainhash.Hash]*State
	tried    map[[32]byte]retry
	lastTip  chainhash.Hash
	lastFull time.Time

	// reread holds the tokens whose registry Refresh has read again, which
	// keep what shows until then.
	reread map[chainhash.Hash]struct{}

	// lastAsked is how many tokens had been asked about at the last cycle.
	lastAsked int

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
		cfg:    cfg,
		views:  make(map[chainhash.Hash]TokenInfo),
		wanted: make(map[chainhash.Hash]bool),
		states: states,
		asked:  make(map[chainhash.Hash]struct{}),
		tried:  make(map[[32]byte]retry),
		reread: make(map[chainhash.Hash]struct{}),
		wake:   make(chan struct{}, 1),
	}
	for _, state := range states {
		m.views[state.Category] = m.viewOf(state)
	}
	return m, nil
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
		if (tracked || info.InHints) && !asked {
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

// Refresh fetches a token's metadata again, or for the first time, whether it
// loads by itself or not.
func (m *Manager) Refresh(category chainhash.Hash) {
	m.mu.Lock()
	m.wanted[category] = true
	m.asked[category] = struct{}{}
	m.mu.Unlock()
	m.signal()
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
		case <-m.wake:
		case <-ticker.C:
		}
		if err := m.cycle(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Warnf("Unable to follow the wallet's tokens: %v", err)
		}
	}
}

// cycle takes on what was asked for, shows what's published as soon as it's
// found, then follows the identities to the tip.  A Status is only left by a
// cycle that reaches its end.
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
	m.fetchRegistries(ctx, states)

	if err := m.cfg.Backend.Advance(ctx, states, m.save); err != nil {
		return err
	}
	m.fetchRegistries(ctx, states)
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
			state.RegistryFailed = false
			if state.Status == StatusFailed || state.Status == StatusUnavailable {
				state.Status = StatusSearching
			}
			if pub := state.CurrentPublication(); pub != nil {
				delete(m.tried, pub.Hash)
			}
		}
	}
	return asked || len(wanted) != 0
}

// fetchRegistries reads the registry of each state whose publication changed.
func (m *Manager) fetchRegistries(ctx context.Context, states []*State) {
	for _, state := range states {
		if ctx.Err() != nil {
			return
		}
		pub := state.CurrentPublication()
		if pub == nil {
			if state.Metadata != nil || state.RegistryHash != [32]byte{} || state.RegistryFailed {
				state.clearRegistry()
				m.save(state)
			}
			continue
		}
		same := pub.Hash == state.RegistryHash && state.RegistryHash != [32]byte{}
		_, reread := m.reread[state.Category]
		if same && !reread && (!state.RegistryChanged ||
			time.Since(state.RegistryRead) < changedRegistryInterval) {

			// A state from before failures were kept has nothing to show.
			if state.Metadata == nil && !state.RegistryFailed {
				state.RegistryFailed = true
				m.save(state)
			}
			continue
		}

		// What showed was from another publication, which this one
		// replaced, so it no longer counts.
		if !same && state.Metadata != nil {
			state.clearRegistry()
			m.save(state)
		}
		if failed, ok := m.tried[pub.Hash]; ok && time.Since(failed.last) < retryWait(failed.tries) {
			continue
		}
		data, err := m.cfg.Fetcher.Fetch(ctx, pub.Hash, pub.URIs)
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

		// Only a collection adds to its registry as it goes, so a file that
		// isn't the published one counts for nothing else.  Its decimals
		// don't count either, as they would change how amounts read.
		if changed {
			if metadata == nil || len(metadata.NFTTypes) == 0 {
				err, metadata = fmt.Errorf("%w, for a token that isn't a collection", err), nil
			} else {
				err = nil
				metadata.Decimals = 0
			}
		}
		if err != nil {
			failed := m.tried[pub.Hash]
			m.tried[pub.Hash] = retry{time.Now(), failed.tries + 1}
			log.Debugf("Unable to read the registry of token %v: %v", state.Category, err)
			if state.Metadata == nil && !state.RegistryFailed {
				state.RegistryFailed = true
				m.save(state)
			}
			continue
		}
		delete(m.tried, pub.Hash)
		delete(m.reread, state.Category)
		state.RegistryHash, state.Metadata = pub.Hash, metadata
		state.RegistryChanged, state.RegistryFailed = changed, metadata == nil
		state.RegistryRead = time.Time{}
		if changed {
			state.RegistryRead = time.Now()
		}
		m.save(state)
	}
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
	m.mu.Lock()
	m.views[state.Category] = view
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
		info.LookAlike = m.looksKnown(info.Metadata)
	}
	return info
}

// looksKnown returns whether metadata has the name or symbol of a Known token.
func (m *Manager) looksKnown(metadata *Metadata) bool {
	for _, known := range m.cfg.Known {
		if strings.EqualFold(known.Name, metadata.Name) ||
			strings.EqualFold(known.Symbol, metadata.Symbol) {

			return true
		}
	}
	return false
}
