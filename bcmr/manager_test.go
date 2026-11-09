package bcmr

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gcash/bchd/chaincfg/chainhash"
)

// fakeBackend gives states the publication of a token by its category.
type fakeBackend struct {
	mu       sync.Mutex
	pubs     map[chainhash.Hash]*Publication
	prepared []chainhash.Hash
	advance  func(states []*State, save func(*State)) error
}

func (b *fakeBackend) Prepare(_ context.Context, states []*State) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, state := range states {
		b.prepared = append(b.prepared, state.Category)
		if pub, ok := b.pubs[state.Category]; ok && len(state.Links) == 0 {
			state.Links = []Link{{Height: 1, Publication: pub}}
		}
	}
	return nil
}

func (b *fakeBackend) Advance(_ context.Context, states []*State, save func(*State)) error {
	if b.advance != nil {
		return b.advance(states, save)
	}
	for _, state := range states {
		if len(state.Links) != 0 {
			state.Status = StatusVerified
			save(state)
		}
	}
	return nil
}

func (b *fakeBackend) preparedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.prepared)
}

// managerFixture is a Manager with a registry server.
type managerFixture struct {
	m       *Manager
	backend *fakeBackend
	hits    *atomic.Int32
	url     string
	hash    [32]byte
}

func registryFor(category chainhash.Hash, name, symbol string) []byte {
	return []byte(fmt.Sprintf(`{"identities":{"%s":{"2024-01-01T00:00:00.000Z":
		{"name":"%s","token":{"category":"%s","symbol":"%s","decimals":2}}}}}`,
		category, name, category, symbol))
}

func newManagerFixture(t *testing.T, category chainhash.Hash, registry []byte,
	known map[chainhash.Hash]*Metadata, auto func(chainhash.Hash) bool) *managerFixture {

	t.Helper()
	hits := new(atomic.Int32)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(registry)
	}))
	t.Cleanup(server.Close)
	store, err := NewStore(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(registry)
	backend := &fakeBackend{pubs: map[chainhash.Hash]*Publication{
		category: {Hash: hash, URIs: []string{server.URL + "/r"}},
	}}
	m, err := NewManager(Config{
		Backend:  backend,
		Store:    store,
		Fetcher:  newFetcher(nil, "", server.Client().Transport),
		Known:    known,
		AutoLoad: auto,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &managerFixture{m, backend, hits, server.URL + "/r", hash}
}

// waitReads hands the Manager each registry read that comes back, and those they
// start in turn, as the run goroutine does, until none is under way.
func waitReads(m *Manager) error {
	for len(m.flights) > 0 {
		select {
		case read := <-m.results:
			m.finishRead(context.Background(), read)
		case <-time.After(time.Minute):
			return errors.New("a registry read didn't come back")
		}
	}
	return nil
}

// cycleAndRead runs a cycle, and waits for the registry reads it started.
func cycleAndRead(m *Manager) error {
	err := m.cycle(context.Background())
	if waitErr := waitReads(m); err == nil {
		err = waitErr
	}
	return err
}

// readRegistries starts the registry reads of the states, as a cycle does, and
// waits for them.
func readRegistries(t *testing.T, m *Manager, states []*State) {
	t.Helper()
	m.startReads(context.Background(), states)
	if err := waitReads(m); err != nil {
		t.Fatal(err)
	}
}

// always loads every token by itself.
func always(chainhash.Hash) bool { return true }

// never loads no token by itself.
func never(chainhash.Hash) bool { return false }

// TestManagerAutoLoad ensures a token that loads by itself gets its metadata
// from being asked about, and is Verified after the cycle.
func TestManagerAutoLoad(t *testing.T) {
	category := chainhash.Hash{1}
	f := newManagerFixture(t, category, registryFor(category, "Token", "TOK"), nil, always)

	infos := f.m.Tokens([]chainhash.Hash{category})
	if len(infos) != 1 || infos[0].Metadata != nil || !infos[0].InHints {
		t.Fatalf("got %+v", infos)
	}
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	info := f.m.Tokens([]chainhash.Hash{category})[0]
	if info.Metadata == nil || info.Metadata.Name != "Token" || info.Metadata.Decimals != 2 ||
		info.Status != StatusVerified || info.LookAlike || info.Known {

		t.Fatalf("got %+v", info)
	}
}

// TestManagerManualToken ensures a token that doesn't load by itself costs
// nothing until Refresh asks for it: being asked about isn't enough, which is
// what keeps a token a stranger sent from doing anything.
func TestManagerManualToken(t *testing.T) {
	category := chainhash.Hash{1}
	f := newManagerFixture(t, category, registryFor(category, "Token", "TOK"), nil, never)

	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	if f.backend.preparedCount() != 0 || f.hits.Load() != 0 {
		t.Fatalf("%d prepared, %d downloads", f.backend.preparedCount(), f.hits.Load())
	}

	if err := f.m.Refresh(category); err != nil {
		t.Fatal(err)
	}
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	info := f.m.Tokens([]chainhash.Hash{category})[0]
	if info.Metadata == nil || info.Status != StatusVerified {
		t.Fatalf("got %+v", info)
	}

	// Once asked for, it's followed from then on.
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	if f.backend.preparedCount() != 2 {
		t.Fatalf("%d prepared", f.backend.preparedCount())
	}
}

// TestManagerUnverifiedFirst ensures metadata shows before the scan ends, as Not
// yet verified.
func TestManagerUnverifiedFirst(t *testing.T) {
	category := chainhash.Hash{1}
	f := newManagerFixture(t, category, registryFor(category, "Token", "TOK"), nil, always)
	var during TokenInfo
	f.backend.advance = func(states []*State, save func(*State)) error {
		// A scan saves as it goes, which takes the reads that came back.
		deadline := time.Now().Add(time.Minute)
		for during.Metadata == nil && time.Now().Before(deadline) {
			save(states[0])
			during = f.m.Tokens([]chainhash.Hash{category})[0]
			time.Sleep(time.Millisecond)
		}
		states[0].Status = StatusVerified
		save(states[0])
		return nil
	}
	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	if during.Metadata == nil || during.Status != StatusSearching {
		t.Fatalf("during the scan: %+v", during)
	}
}

// TestManagerFetchesOnce ensures a registry isn't read again for the same hash,
// and is read again by a Refresh.
func TestManagerFetchesOnce(t *testing.T) {
	category := chainhash.Hash{1}
	f := newManagerFixture(t, category, registryFor(category, "Token", "TOK"), nil, always)
	f.m.Tokens([]chainhash.Hash{category})
	for range 3 {
		if err := cycleAndRead(f.m); err != nil {
			t.Fatal(err)
		}
	}
	if f.hits.Load() != 1 {
		t.Fatalf("%d downloads", f.hits.Load())
	}
	if err := f.m.Refresh(category); err != nil {
		t.Fatal(err)
	}
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 2 {
		t.Fatalf("%d downloads after a refresh", f.hits.Load())
	}
}

// TestManagerSkipsIdleCycles ensures no new block and nothing new costs nothing,
// and that a new block does.
func TestManagerSkipsIdleCycles(t *testing.T) {
	category := chainhash.Hash{1}
	f := newManagerFixture(t, category, registryFor(category, "Token", "TOK"), nil, always)
	tip := chainhash.Hash{1}
	f.m.cfg.Tip = func() (*chainhash.Hash, error) { return &tip, nil }
	f.m.Tokens([]chainhash.Hash{category})
	for range 3 {
		if err := cycleAndRead(f.m); err != nil {
			t.Fatal(err)
		}
	}
	if f.backend.preparedCount() != 1 {
		t.Fatalf("%d prepared", f.backend.preparedCount())
	}
	tip = chainhash.Hash{2}
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	if f.backend.preparedCount() != 2 {
		t.Fatalf("%d prepared after a block", f.backend.preparedCount())
	}
}

// TestManagerOnlyAskedTokens ensures a token the app no longer shows, as a hidden
// one, isn't followed after a restart, and is when it shows again.
func TestManagerOnlyAskedTokens(t *testing.T) {
	category := chainhash.Hash{1}
	f := newManagerFixture(t, category, registryFor(category, "Token", "TOK"), nil, always)
	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(f.m.cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.backend.prepared = nil
	if err := cycleAndRead(m); err != nil {
		t.Fatal(err)
	}
	if f.backend.preparedCount() != 0 {
		t.Fatal("a token that isn't shown was followed")
	}
	m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(m); err != nil {
		t.Fatal(err)
	}
	if f.backend.preparedCount() != 1 {
		t.Fatal("a token that shows again wasn't followed")
	}
}

// TestManagerNewPublicationReplaces ensures what showed from an old publication
// doesn't stay when a new one can't be read, or when none is left: a registry
// that fails its hash counts as no metadata.
func TestManagerNewPublicationReplaces(t *testing.T) {
	category := chainhash.Hash{1}
	f := newManagerFixture(t, category, registryFor(category, "Token", "TOK"), nil, always)
	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	state := f.m.states[category]
	state.Links = []Link{{Height: 1, Publication: &Publication{Hash: [32]byte{1},
		URIs: []string{f.url}}}}
	readRegistries(t, f.m, []*State{state})
	if info := f.m.views[category]; info.Metadata != nil {
		t.Fatalf("got %+v", info)
	}

	f.backend.pubs = nil
	state.Metadata, state.RegistryHash = &Metadata{Name: "Old"}, [32]byte{9}
	state.Links = []Link{{Height: 1}}
	readRegistries(t, f.m, []*State{state})
	if info := f.m.views[category]; info.Metadata != nil {
		t.Fatalf("with no publication: %+v", info)
	}
}

// TestRetryWait ensures a registry that keeps failing is asked for less and less.
func TestRetryWait(t *testing.T) {
	if retryWait(1) != retryInterval || retryWait(2) != 2*retryInterval {
		t.Fatalf("%v, %v", retryWait(1), retryWait(2))
	}
	if retryWait(9) != maxRetryInterval || retryWait(100) != maxRetryInterval {
		t.Fatal("no limit")
	}
}

// TestManagerRetriesLater ensures a registry that can't be read isn't asked for
// on every cycle, and that a Refresh tries at once.
func TestManagerRetriesLater(t *testing.T) {
	category := chainhash.Hash{1}
	f := newManagerFixture(t, category, registryFor(category, "Token", "TOK"), nil, always)
	f.backend.pubs[category].Hash = [32]byte{1} // Nothing will match it.
	f.m.Tokens([]chainhash.Hash{category})
	for range 3 {
		if err := cycleAndRead(f.m); err != nil {
			t.Fatal(err)
		}
	}
	if f.hits.Load() != 1 {
		t.Fatalf("%d downloads", f.hits.Load())
	}
	if err := f.m.Refresh(category); err != nil {
		t.Fatal(err)
	}
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 2 {
		t.Fatalf("%d downloads after a refresh", f.hits.Load())
	}
}

// TestManagerNoUsableMetadata ensures a registry that says nothing usable is
// taken as an answer, and isn't downloaded again.
func TestManagerNoUsableMetadata(t *testing.T) {
	category := chainhash.Hash{1}
	f := newManagerFixture(t, category, registryFor(chainhash.Hash{9}, "Other", "OTH"), nil, always)
	f.m.Tokens([]chainhash.Hash{category})
	for range 2 {
		if err := cycleAndRead(f.m); err != nil {
			t.Fatal(err)
		}
	}
	info := f.m.Tokens([]chainhash.Hash{category})[0]
	if info.Metadata != nil || f.hits.Load() != 1 {
		t.Fatalf("got %+v after %d downloads", info, f.hits.Load())
	}
}

// TestManagerKnown ensures a Known token shows at once from the list, and that
// verified data from its registry then wins, keeping the Known flag.
func TestManagerKnown(t *testing.T) {
	category := chainhash.Hash{1}
	known := map[chainhash.Hash]*Metadata{category: {Name: "Listed", Symbol: "LST", Decimals: 8}}
	f := newManagerFixture(t, category, registryFor(category, "Published", "PUB"), known, always)

	info := f.m.Tokens([]chainhash.Hash{category})[0]
	if !info.Known || info.Metadata == nil || info.Metadata.Name != "Listed" {
		t.Fatalf("before: %+v", info)
	}
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	info = f.m.Tokens([]chainhash.Hash{category})[0]
	if !info.Known || info.Metadata.Name != "Published" || info.LookAlike {
		t.Fatalf("after: %+v", info)
	}
}

// TestManagerLookAlike ensures a token with a Known token's name or symbol,
// under another category, is flagged, whatever its case, also while unverified.
func TestManagerLookAlike(t *testing.T) {
	known := map[chainhash.Hash]*Metadata{{9}: {Name: "Real Token", Symbol: "REAL"}}
	category := chainhash.Hash{1}
	for _, test := range []struct {
		name, symbol string
		want         bool
	}{
		{"real token", "FAKE", true},
		{"Other", "REAL", true},
		{"Other", "OTHER", false},
	} {
		f := newManagerFixture(t, category, registryFor(category, test.name, test.symbol), known, always)
		f.backend.advance = func([]*State, func(*State)) error { return nil }
		f.m.Tokens([]chainhash.Hash{category})
		if err := cycleAndRead(f.m); err != nil {
			t.Fatal(err)
		}
		info := f.m.Tokens([]chainhash.Hash{category})[0]
		if info.LookAlike != test.want || info.Status != StatusSearching {
			t.Fatalf("%+v: got %+v", test, info)
		}
	}
}

// TestManagerRestart ensures what was found outlives a restart, with nothing
// fetched again.
func TestManagerRestart(t *testing.T) {
	category := chainhash.Hash{1}
	f := newManagerFixture(t, category, registryFor(category, "Token", "TOK"), nil, always)
	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	cfg := f.m.cfg
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	info := m.Tokens([]chainhash.Hash{category})[0]
	if info.Metadata == nil || info.Metadata.Name != "Token" || info.Status != StatusVerified {
		t.Fatalf("got %+v", info)
	}
	if err := cycleAndRead(m); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 1 {
		t.Fatalf("%d downloads", f.hits.Load())
	}
}

// TestManagerNotReady ensures nothing happens until the chain is ready.
func TestManagerNotReady(t *testing.T) {
	category := chainhash.Hash{1}
	f := newManagerFixture(t, category, registryFor(category, "Token", "TOK"), nil, always)
	f.m.cfg.Ready = func() bool { return false }
	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	if f.backend.preparedCount() != 0 {
		t.Fatal("the backend was asked")
	}
}

// TestManagerAdvanceError ensures a failed scan keeps what was found.
func TestManagerAdvanceError(t *testing.T) {
	category := chainhash.Hash{1}
	f := newManagerFixture(t, category, registryFor(category, "Token", "TOK"), nil, always)
	f.backend.advance = func([]*State, func(*State)) error { return errors.New("no filter") }
	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err == nil {
		t.Fatal("no error")
	}
	info := f.m.Tokens([]chainhash.Hash{category})[0]
	if info.Metadata == nil || info.Status != StatusSearching {
		t.Fatalf("got %+v", info)
	}
}

// TestManagerStartStop ensures the goroutine picks up work and stops.
func TestManagerStartStop(t *testing.T) {
	category := chainhash.Hash{1}
	f := newManagerFixture(t, category, registryFor(category, "Token", "TOK"), nil, always)
	f.m.Start()
	f.m.Tokens([]chainhash.Hash{category})
	deadline := time.Now().Add(30 * time.Second)
	for f.m.Tokens([]chainhash.Hash{category})[0].Status != StatusVerified {
		if time.Now().After(deadline) {
			t.Fatal("the token wasn't resolved")
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.m.Stop()
}

// collectionFor is a registry of a collection with decimals it shouldn't have.
func collectionFor(category chainhash.Hash, name string) []byte {
	return []byte(fmt.Sprintf(`{"identities":{"%s":{"2024-01-01T00:00:00.000Z":
		{"name":"%s","token":{"category":"%s","symbol":"C","decimals":8,
		"nfts":{"parse":{"types":{"01":{"name":"One"}}}}}}}}}`, category, name, category))
}

// changedFixture is a fixture whose host serves file under a hash of another.
func changedFixture(t *testing.T, category chainhash.Hash, file []byte) *managerFixture {
	t.Helper()
	f := newManagerFixture(t, category, file, nil, always)
	f.backend.pubs[category].Hash = sha256.Sum256([]byte("the whole set"))
	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestManagerChangedCollection ensures a collection's registry that isn't the
// published one shows, marked, with no decimals, and is read again only later
// or when asked.
func TestManagerChangedCollection(t *testing.T) {
	category := chainhash.Hash{0x31}
	f := changedFixture(t, category, collectionFor(category, "Minting"))
	info := f.m.Tokens([]chainhash.Hash{category})[0]
	if info.Metadata == nil || !info.RegistryChanged || info.RegistryFailed {
		t.Fatalf("%+v", info)
	}
	if info.Metadata.Decimals != 0 || info.Metadata.NFTTypes["01"].Name != "One" {
		t.Fatalf("%+v", info.Metadata)
	}

	hits := f.hits.Load()
	state := f.m.states[category]
	readRegistries(t, f.m, []*State{state})
	if f.hits.Load() != hits {
		t.Fatal("read again right away")
	}
	state.RegistryRead = time.Now().Add(-changedRegistryInterval - time.Minute)
	readRegistries(t, f.m, []*State{state})
	if f.hits.Load() != hits+1 {
		t.Fatal("not read again after the interval")
	}

	if err := f.m.Refresh(category); err != nil {
		t.Fatal(err)
	}
	f.m.takeWanted()
	if state.Metadata == nil {
		t.Fatal("Refresh dropped what shows, which no cache can bring back")
	}
	readRegistries(t, f.m, []*State{state})
	if f.hits.Load() != hits+2 {
		t.Fatal("not read again when asked")
	}
}

// TestManagerChangedFungible ensures a registry that isn't the published one
// counts for nothing if the token isn't a collection.
func TestManagerChangedFungible(t *testing.T) {
	category := chainhash.Hash{0x32}
	f := changedFixture(t, category, registryFor(category, "Coin", "COIN"))
	info := f.m.Tokens([]chainhash.Hash{category})[0]
	if info.Metadata != nil || info.RegistryChanged || !info.RegistryFailed {
		t.Fatalf("%+v", info)
	}
}

// TestManagerRegistryFailed ensures a registry that can't be read is reported,
// and that the report ends once it can be.
func TestManagerRegistryFailed(t *testing.T) {
	category := chainhash.Hash{0x33}
	file := registryFor(category, "Coin", "COIN")
	f := newManagerFixture(t, category, file, nil, always)
	uris := f.backend.pubs[category].URIs
	f.backend.pubs[category].URIs = []string{"https://127.0.0.1:1/r"}
	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	if info := f.m.Tokens([]chainhash.Hash{category})[0]; !info.RegistryFailed {
		t.Fatalf("%+v", info)
	}
	state := f.m.states[category]
	for i := range state.Links {
		state.Links[i].Publication.URIs = uris
	}
	if err := f.m.Refresh(category); err != nil {
		t.Fatal(err)
	}
	f.m.takeWanted()
	readRegistries(t, f.m, []*State{state})
	if info := f.m.Tokens([]chainhash.Hash{category})[0]; info.RegistryFailed || info.Metadata == nil {
		t.Fatalf("%+v", info)
	}
}

// TestManagerKnownOverChanged ensures the Known list wins over a file that isn't
// the published one.
func TestManagerKnownOverChanged(t *testing.T) {
	category := chainhash.Hash{0x34}
	known := map[chainhash.Hash]*Metadata{category: {Name: "Listed", Symbol: "L"}}
	f := newManagerFixture(t, category, collectionFor(category, "Host"), known, always)
	f.backend.pubs[category].Hash = sha256.Sum256([]byte("the whole set"))
	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	info := f.m.Tokens([]chainhash.Hash{category})[0]
	if info.Metadata == nil || info.Metadata.Name != "Listed" || info.RegistryChanged {
		t.Fatalf("%+v", info)
	}
}

// TestManagerRefreshKeepsWhatShows ensures a Refresh that can't read the registry
// again leaves a token as it was.
func TestManagerRefreshKeepsWhatShows(t *testing.T) {
	category := chainhash.Hash{0x35}
	f := newManagerFixture(t, category, registryFor(category, "Coin", "COIN"), nil, always)
	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	state := f.m.states[category]
	for i := range state.Links {
		state.Links[i].Publication.URIs = []string{"https://127.0.0.1:1/r"}
	}
	if err := f.m.Refresh(category); err != nil {
		t.Fatal(err)
	}
	f.m.takeWanted()
	readRegistries(t, f.m, []*State{state})
	info := f.m.Tokens([]chainhash.Hash{category})[0]
	if info.Metadata == nil || info.RegistryFailed {
		t.Fatalf("%+v", info)
	}
}

// TestManagerOldFailure ensures a state saved with a registry that had nothing
// to use, from before such failures were kept, reports one.
func TestManagerOldFailure(t *testing.T) {
	category := chainhash.Hash{0x36}
	f := newManagerFixture(t, category, registryFor(category, "Coin", "COIN"), nil, always)
	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	state := f.m.states[category]
	state.Metadata = nil
	readRegistries(t, f.m, []*State{state})
	if info := f.m.Tokens([]chainhash.Hash{category})[0]; !info.RegistryFailed {
		t.Fatalf("%+v", info)
	}
}

// TestManagerNewParser ensures a registry read by an older parser is read again.
func TestManagerNewParser(t *testing.T) {
	category := chainhash.Hash{0x37}
	f := newManagerFixture(t, category, registryFor(category, "Coin", "COIN"), nil, always)
	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	state := f.m.states[category]
	hits := f.hits.Load()
	state.RegistryParser = 0
	state.Metadata.Name = "Old"
	readRegistries(t, f.m, []*State{state})
	if f.hits.Load() != hits+1 || state.Metadata.Name != "Coin" || state.RegistryParser != registryParser {
		t.Fatalf("%d hits, %+v", f.hits.Load()-hits, state.Metadata)
	}
	readRegistries(t, f.m, []*State{state})
	if f.hits.Load() != hits+1 {
		t.Fatal("read again")
	}
}

// TestManagerReadAgainKeepsWhatShows ensures a registry that a new parser can't
// use leaves a token as it was, and is tried again.
func TestManagerReadAgainKeepsWhatShows(t *testing.T) {
	category := chainhash.Hash{0x38}
	f := newManagerFixture(t, category, []byte(`{"identities":{}}`), nil, always)
	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	state := f.m.states[category]
	state.RegistryHash = f.backend.pubs[category].Hash
	state.Metadata = &Metadata{Name: "Shown", Symbol: "S"}
	state.RegistryFailed, state.RegistryParser = false, 0
	readRegistries(t, f.m, []*State{state})
	if state.Metadata == nil || state.RegistryFailed || state.RegistryParser == registryParser {
		t.Fatalf("%+v", state)
	}
}

// TestManagerReservedSymbol ensures a token with a symbol that reads as money is
// a look-alike, also with no Known list, and that a class of a Known token's
// symbol is too.
func TestManagerReservedSymbol(t *testing.T) {
	m := &Manager{cfg: Config{Known: map[chainhash.Hash]*Metadata{{1}: {Name: "Known", Symbol: "MUSD"}, {2}: {Name: "Listed", Symbol: "USDT"}}}}
	for symbol, want := range map[string][2]bool{"BCH": {true, true}, "BCH-X": {true, true}, "USD": {true, true},
		"DOGE": {true, true}, "MUSD-A": {true, false}, "OTHER": {false, false}, "TOP": {false, false}} {

		state := &State{Category: chainhash.Hash{9}, Metadata: &Metadata{Name: "N", Symbol: symbol}}
		info := m.viewOf(state)
		if info.LookAlike != want[0] || info.ReservedSymbol != want[1] {
			t.Errorf("%q: %+v", symbol, info)
		}
	}
	state := &State{Category: chainhash.Hash{2}, Metadata: &Metadata{Name: "Listed", Symbol: "USDT"}}
	if info := m.viewOf(state); info.LookAlike || info.ReservedSymbol {
		t.Errorf("a Known token: %+v", info)
	}
}

// TestManagerChangedFileKeepsPublished ensures a file that isn't the published one
// doesn't replace what was read of the published one.
func TestManagerChangedFileKeepsPublished(t *testing.T) {
	category := chainhash.Hash{0x39}
	f := newManagerFixture(t, category, collectionFor(category, "Host"), nil, always)
	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	state := f.m.states[category]
	state.Links[0].Publication.Hash = sha256.Sum256([]byte("published"))
	state.RegistryHash = state.Links[0].Publication.Hash
	state.Metadata = &Metadata{Name: "Published", Symbol: "P", Decimals: 8}
	state.RegistryParser = 0
	readRegistries(t, f.m, []*State{state})
	if state.Metadata.Name != "Published" || state.Metadata.Decimals != 8 || state.RegistryChanged {
		t.Fatalf("%+v", state.Metadata)
	}
}

// TestManagerNextSnapshot ensures a registry is read again once a snapshot still
// to come has taken over.
func TestManagerNextSnapshot(t *testing.T) {
	category := chainhash.Hash{0x3a}
	f := newManagerFixture(t, category, registryFor(category, "Coin", "COIN"), nil, always)
	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	state := f.m.states[category]
	hits := f.hits.Load()
	state.Metadata.NextSnapshot = time.Now().Add(time.Hour)
	readRegistries(t, f.m, []*State{state})
	if f.hits.Load() != hits {
		t.Fatal("read before its time")
	}
	state.Metadata.NextSnapshot = time.Now().Add(-time.Hour)
	readRegistries(t, f.m, []*State{state})
	if state.Metadata == nil || !state.Metadata.NextSnapshot.IsZero() {
		t.Fatalf("%+v", state.Metadata)
	}
}

// TestManagerZeroHash ensures a publication of a hash of zeros isn't read over and
// over.
func TestManagerZeroHash(t *testing.T) {
	category := chainhash.Hash{0x3b}
	f := newManagerFixture(t, category, collectionFor(category, "Host"), nil, always)
	f.backend.pubs[category].Hash = [32]byte{}
	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	state := f.m.states[category]
	hits := f.hits.Load()
	readRegistries(t, f.m, []*State{state})
	if f.hits.Load() != hits || state.Metadata == nil {
		t.Fatalf("%d more reads, %+v", f.hits.Load()-hits, state.Metadata)
	}
}

// TestManagerPeek ensures Peek says what's known and has nothing load.
func TestManagerPeek(t *testing.T) {
	category := chainhash.Hash{0x3c}
	f := newManagerFixture(t, category, registryFor(category, "Coin", "COIN"), nil, always)
	if info := f.m.Peek([]chainhash.Hash{category})[0]; info.Metadata != nil {
		t.Fatalf("%+v", info)
	}
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 0 || len(f.m.asked) != 0 {
		t.Fatal("Peek had the token load")
	}

	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	if info := f.m.Peek([]chainhash.Hash{category})[0]; info.Metadata == nil || info.Metadata.Name != "Coin" {
		t.Fatalf("%+v", info)
	}
}

// TestManagerNoAutoLoadLeavesStoredTokens ensures a token stored earlier neither
// loads nor is followed with nothing loading by itself, but still shows.
func TestManagerNoAutoLoadLeavesStoredTokens(t *testing.T) {
	category := chainhash.Hash{0x3d}
	f := newManagerFixture(t, category, registryFor(category, "Coin", "COIN"), nil, always)
	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Config{Backend: f.backend, Store: f.m.cfg.Store, Fetcher: f.m.cfg.Fetcher})
	if err != nil {
		t.Fatal(err)
	}
	hits := f.hits.Load()
	info := m.Tokens([]chainhash.Hash{category})[0]
	if info.Metadata == nil || info.Metadata.Name != "Coin" {
		t.Fatalf("%+v", info)
	}
	m.states[category].RegistryParser = 0
	if err := cycleAndRead(m); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != hits || len(m.asked) != 0 {
		t.Fatalf("%d more reads", f.hits.Load()-hits)
	}
}

// TestManagerRetriesApart ensures tokens that publish one registry don't share a
// failure.
func TestManagerRetriesApart(t *testing.T) {
	coin, collection := chainhash.Hash{0x3e}, chainhash.Hash{0x3f}
	f := newManagerFixture(t, collection, collectionFor(collection, "Host"), nil, always)
	pub := &Publication{Hash: sha256.Sum256([]byte("the whole set")), URIs: f.backend.pubs[collection].URIs}
	coinState := &State{Category: coin, Links: []Link{{Height: 1, Publication: pub}}}
	state := &State{Category: collection, Links: []Link{{Height: 1, Publication: pub}}}
	readRegistries(t, f.m, []*State{coinState, state})
	if coinState.Metadata != nil || state.Metadata == nil {
		t.Fatalf("%+v, %+v", coinState.Metadata, state.Metadata)
	}
}

// readsFixture is a Manager with a token for each registry, served by category
// after hold returns, and the number of downloads.
func readsFixture(t *testing.T, registries map[chainhash.Hash][]byte,
	hold func(chainhash.Hash)) (*Manager, *atomic.Int32) {

	t.Helper()
	hits := new(atomic.Int32)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		category, err := chainhash.NewHashFromStr(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil {
			t.Error(err)
			return
		}
		hits.Add(1)
		hold(*category)
		_, _ = w.Write(registries[*category])
	}))
	t.Cleanup(server.Close)
	store, err := NewStore(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	pubs := make(map[chainhash.Hash]*Publication)
	for category, registry := range registries {
		pubs[category] = &Publication{
			Hash: sha256.Sum256(registry),
			URIs: []string{server.URL + "/" + category.String()},
		}
	}
	m, err := NewManager(Config{
		Backend:  &fakeBackend{pubs: pubs},
		Store:    store,
		Fetcher:  newFetcher(nil, t.TempDir(), server.Client().Transport),
		AutoLoad: always,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, hits
}

// TestManagerSlowHostHoldsNoOthers ensures a token shows while the host of
// another one is still answering, whichever is read first.
func TestManagerSlowHostHoldsNoOthers(t *testing.T) {
	slow, fast := chainhash.Hash{1}, chainhash.Hash{2}
	release := make(chan struct{})

	// Each is only served once both are being read, so that reading them one
	// at a time can't pass, whichever comes first.
	slowReading := make(chan struct{})
	m, _ := readsFixture(t, map[chainhash.Hash][]byte{
		slow: registryFor(slow, "Slow", "SLOW"),
		fast: registryFor(fast, "Fast", "FAST"),
	}, func(category chainhash.Hash) {
		if category == slow {
			close(slowReading)
			<-release
			return
		}
		select {
		case <-slowReading:
		case <-release:
		}
	})
	m.Tokens([]chainhash.Hash{slow, fast})
	done := make(chan error)
	go func() { done <- cycleAndRead(m) }()

	deadline := time.Now().Add(30 * time.Second)
	for m.Peek([]chainhash.Hash{fast})[0].Metadata == nil {
		if time.Now().After(deadline) {
			close(release)
			<-done
			t.Fatal("a slow host held up another token")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if m.Peek([]chainhash.Hash{slow})[0].Metadata != nil {
		t.Fatal("the slow host's token showed early")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if m.Peek([]chainhash.Hash{slow})[0].Metadata == nil {
		t.Fatal("the slow host's token didn't show")
	}
}

// TestManagerReadsAllAtOnce ensures registries are read at once with no limit
// but the Fetcher's at each host: as many at once from two hosts as each allows.
func TestManagerReadsAllAtOnce(t *testing.T) {
	registries := make(map[chainhash.Hash][]byte)
	var categories []chainhash.Hash
	for i := range 2 * maxPerHost {
		category := chainhash.Hash{byte(i + 1)}
		registries[category] = registryFor(category, "Token", "TOK")
		categories = append(categories, category)
	}
	arrived := make(chan struct{}, len(categories))
	release := make(chan struct{})
	hold := func(chainhash.Hash) {
		arrived <- struct{}{}
		<-release
	}
	m, _ := readsFixture(t, registries, hold)

	// Half the tokens are at a second host.
	second := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		category, err := chainhash.NewHashFromStr(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil {
			t.Error(err)
			return
		}
		hold(*category)
		_, _ = w.Write(registries[*category])
	}))
	t.Cleanup(second.Close)
	pubs := m.cfg.Backend.(*fakeBackend).pubs
	for _, category := range categories[maxPerHost:] {
		pubs[category].URIs = []string{second.URL + "/" + category.String()}
	}

	m.Tokens(categories)
	if err := m.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range categories {
		select {
		case <-arrived:
		case <-time.After(time.Minute):
			close(release)
			t.Fatal("fewer registries read at once than the hosts allow")
		}
	}
	close(release)
	if err := waitReads(m); err != nil {
		t.Fatal(err)
	}
	for i, info := range m.Peek(categories) {
		if info.Metadata == nil {
			t.Errorf("token %d didn't show", i)
		}
	}
}

// sharedRegistry is a registry of both tokens.
func sharedRegistry(first, second chainhash.Hash) []byte {
	return []byte(fmt.Sprintf(`{"identities":{
		"%[1]s":{"2024-01-01T00:00:00.000Z":{"name":"One","token":{"category":"%[1]s","symbol":"ONE"}}},
		"%[2]s":{"2024-01-01T00:00:00.000Z":{"name":"Two","token":{"category":"%[2]s","symbol":"TWO"}}}}}`,
		first, second))
}

// TestManagerSharedRegistryReadOnce ensures tokens with one registry at the same
// URIs don't each download it, with no cache to find it in, and that it serves
// them all.
func TestManagerSharedRegistryReadOnce(t *testing.T) {
	first, second := chainhash.Hash{1}, chainhash.Hash{2}
	shared := sharedRegistry(first, second)
	m, hits := readsFixture(t, map[chainhash.Hash][]byte{first: shared, second: shared},
		func(chainhash.Hash) {})
	m.cfg.Fetcher.CacheDir = ""
	pubs := m.cfg.Backend.(*fakeBackend).pubs
	pubs[second].URIs = pubs[first].URIs
	m.Tokens([]chainhash.Hash{first, second})
	if err := cycleAndRead(m); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("%d downloads", hits.Load())
	}
	infos := m.Peek([]chainhash.Hash{first, second})
	if infos[0].Metadata == nil || infos[0].Metadata.Name != "One" ||
		infos[1].Metadata == nil || infos[1].Metadata.Name != "Two" {

		t.Fatalf("%+v", infos)
	}
}

// TestManagerSharedRegistryFailing ensures a failed read counts for a token with the
// same URIs, without a download of its own, and not for one with others.
func TestManagerSharedRegistryFailing(t *testing.T) {
	first, second := chainhash.Hash{1}, chainhash.Hash{2}
	shared := sharedRegistry(first, second)
	registries := map[chainhash.Hash][]byte{first: shared, second: shared}
	m, hits := readsFixture(t, registries, func(chainhash.Hash) {})
	pubs := m.cfg.Backend.(*fakeBackend).pubs
	wrong := sha256.Sum256([]byte("not this file"))
	pubs[first].Hash, pubs[second].Hash = wrong, wrong
	m.Tokens([]chainhash.Hash{first, second})
	if err := cycleAndRead(m); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Fatalf("%d downloads with other URIs", hits.Load())
	}

	m, hits = readsFixture(t, registries, func(chainhash.Hash) {})
	pubs = m.cfg.Backend.(*fakeBackend).pubs
	pubs[first].Hash = wrong
	pubs[second].Hash, pubs[second].URIs = wrong, pubs[first].URIs
	m.Tokens([]chainhash.Hash{first, second})
	if err := cycleAndRead(m); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("%d downloads with the same URIs", hits.Load())
	}
	if len(m.tried) != 2 {
		t.Fatalf("%d failures kept", len(m.tried))
	}
}

// TestManagerChangedFileNotShared ensures a file that isn't the published one, which
// counts only for where it came from, isn't taken for a token with other URIs.
func TestManagerChangedFileNotShared(t *testing.T) {
	first, second := chainhash.Hash{1}, chainhash.Hash{2}
	collection := []byte(fmt.Sprintf(`{"identities":{
		"%[1]s":{"2024-01-01T00:00:00.000Z":{"name":"One","token":{"category":"%[1]s","symbol":"ONE",
			"nfts":{"parse":{"bytecode":"","types":{"01":{"name":"A"}}}}}}},
		"%[2]s":{"2024-01-01T00:00:00.000Z":{"name":"Two","token":{"category":"%[2]s","symbol":"TWO",
			"nfts":{"parse":{"bytecode":"","types":{"01":{"name":"A"}}}}}}}}}`, first, second))
	m, hits := readsFixture(t, map[chainhash.Hash][]byte{first: collection, second: collection},
		func(chainhash.Hash) {})
	pubs := m.cfg.Backend.(*fakeBackend).pubs
	wrong := sha256.Sum256([]byte("the whole set"))
	pubs[first].Hash, pubs[second].Hash = wrong, wrong
	m.Tokens([]chainhash.Hash{first, second})
	if err := cycleAndRead(m); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Fatalf("%d downloads", hits.Load())
	}
	for _, info := range m.Peek([]chainhash.Hash{first, second}) {
		if info.Metadata == nil {
			t.Fatalf("%+v", info)
		}
	}
}

// TestManagerStopsDuringReads ensures stopping doesn't wait for hosts, and that
// reads it ended don't count as failures.
func TestManagerStopsDuringReads(t *testing.T) {
	category := chainhash.Hash{1}
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})

	// Before the server closes, which waits for its handlers.
	defer close(release)
	m, _ := readsFixture(t, map[chainhash.Hash][]byte{
		category: registryFor(category, "Token", "TOK"),
	}, func(chainhash.Hash) {
		arrived <- struct{}{}
		<-release
	})
	m.Start()
	m.Tokens([]chainhash.Hash{category})
	select {
	case <-arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the registry wasn't read")
	}
	stopped := make(chan struct{})
	go func() {
		m.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("a read held up stopping")
	}
	if len(m.tried) != 0 {
		t.Fatalf("%d failures", len(m.tried))
	}
}

// TestManagerChangedFileClaimsSnapshot ensures a host can't have a file that isn't
// the published one read at every block by saying a snapshot is due.
func TestManagerChangedFileClaimsSnapshot(t *testing.T) {
	category := chainhash.Hash{0x3b}
	f := changedFixture(t, category, collectionFor(category, "Set"))
	state := f.m.states[category]
	if !state.RegistryChanged || state.Metadata == nil {
		t.Fatalf("%+v", state)
	}
	hits := f.hits.Load()
	state.Metadata.NextSnapshot = time.Now().Add(-time.Hour)
	readRegistries(t, f.m, []*State{state})
	if f.hits.Load() != hits {
		t.Fatal("read for its snapshot")
	}
	state.RegistryRead = time.Now().Add(-2 * changedRegistryInterval)
	readRegistries(t, f.m, []*State{state})
	if f.hits.Load() != hits+1 {
		t.Fatal("not read the next day")
	}
}

// userActionSpy is a transport that counts the requests asked for by the user.
type userActionSpy struct {
	next  http.RoundTripper
	users atomic.Int32
}

func (s *userActionSpy) RoundTrip(req *http.Request) (*http.Response, error) {
	if isUserAction(req.Context()) {
		s.users.Add(1)
	}
	return s.next.RoundTrip(req)
}

// TestManagerRefreshIsUserAction ensures the read of a token the user refreshed is
// the user's, and one that loads by itself isn't.
func TestManagerRefreshIsUserAction(t *testing.T) {
	category := chainhash.Hash{0x51}
	f := newManagerFixture(t, category, registryFor(category, "Token", "TOK"), nil, always)
	spy := &userActionSpy{next: f.m.cfg.Fetcher.client.Transport}
	f.m.cfg.Fetcher.client.Transport = spy
	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	if spy.users.Load() != 0 {
		t.Fatal("a token that loads by itself was the user's")
	}
	if err := f.m.Refresh(category); err != nil {
		t.Fatal(err)
	}
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	if spy.users.Load() != 1 {
		t.Fatalf("%d of the user's", spy.users.Load())
	}
}

// TestManagerReadHoldsNoChain ensures a cycle follows the chain while a registry
// read is under way, rather than wait for it, and that the read shows once it
// comes back.
func TestManagerReadHoldsNoChain(t *testing.T) {
	category := chainhash.Hash{1}
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	m, _ := readsFixture(t, map[chainhash.Hash][]byte{
		category: registryFor(category, "Token", "TOK"),
	}, func(chainhash.Hash) {
		arrived <- struct{}{}
		<-release
	})
	advanced := false
	backend := m.cfg.Backend.(*fakeBackend)
	backend.advance = func(states []*State, save func(*State)) error {
		advanced = true
		for _, state := range states {
			state.Status = StatusVerified
			save(state)
		}
		return nil
	}
	m.Tokens([]chainhash.Hash{category})
	done := make(chan error, 1)
	go func() { done <- m.cycle(context.Background()) }()
	select {
	case err := <-done:
		if err != nil || !advanced {
			t.Fatalf("advanced %v, %v", advanced, err)
		}
	case <-time.After(time.Minute):
		t.Fatal("a registry read held up the chain")
	}
	<-arrived
	if m.Peek([]chainhash.Hash{category})[0].Metadata != nil {
		t.Fatal("the registry showed before it came")
	}
	release <- struct{}{}
	if err := waitReads(m); err != nil {
		t.Fatal(err)
	}
	if info := m.Peek([]chainhash.Hash{category})[0]; info.Metadata == nil || info.Status != StatusVerified {
		t.Fatalf("%+v", info)
	}
}

// TestManagerRefreshDuringRead ensures a Refresh is read at once, while another
// token's read is still under way.
func TestManagerRefreshDuringRead(t *testing.T) {
	slow, asked := chainhash.Hash{1}, chainhash.Hash{2}
	release := make(chan struct{})
	defer close(release)
	m, _ := readsFixture(t, map[chainhash.Hash][]byte{
		slow:  registryFor(slow, "Slow", "SLOW"),
		asked: registryFor(asked, "Asked", "ASK"),
	}, func(category chainhash.Hash) {
		if category == slow {
			<-release
		}
	})
	m.cfg.AutoLoad = func(category chainhash.Hash) bool { return category == slow }

	// A scan that saves nothing leaves the reads to the run loop.
	m.cfg.Backend.(*fakeBackend).advance = func([]*State, func(*State)) error { return nil }
	m.Start()
	defer m.Stop()
	m.Tokens([]chainhash.Hash{slow, asked})
	if err := m.Refresh(asked); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute)
	for m.Peek([]chainhash.Hash{asked})[0].Metadata == nil {
		if time.Now().After(deadline) {
			t.Fatal("a slow read held up a Refresh")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if m.Peek([]chainhash.Hash{slow})[0].Metadata != nil {
		t.Fatal("the slow token showed early")
	}
}

// TestManagerReadOfOldPublication ensures a read that comes back after its token
// published again counts for nothing, and that the new publication is read.
func TestManagerReadOfOldPublication(t *testing.T) {
	category := chainhash.Hash{1}
	newer := registryFor(category, "Newer", "NEW")
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	held := true
	registries := map[chainhash.Hash][]byte{category: registryFor(category, "Older", "OLD")}
	m, _ := readsFixture(t, registries, func(chainhash.Hash) {
		if held {
			held = false
			arrived <- struct{}{}
			<-release
		}
	})
	m.Tokens([]chainhash.Hash{category})
	if err := m.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-arrived

	// The token publishes the newer registry, at the same URI, while the older
	// one is under way.
	state := m.states[category]
	old := *state.Links[0].Publication
	state.Links = append(state.Links, Link{Height: 2, Publication: &Publication{
		Hash: sha256.Sum256(newer), URIs: old.URIs,
	}})
	m.cfg.Backend.(*fakeBackend).pubs[category] = state.Links[1].Publication
	registries[category] = newer
	release <- struct{}{}
	read := <-m.results
	m.finishRead(context.Background(), read)
	if len(m.tried) != 0 {
		t.Fatalf("an old read counted as a failure: %v", m.tried)
	}
	if err := waitReads(m); err != nil {
		t.Fatal(err)
	}
	if info := m.Peek([]chainhash.Hash{category})[0]; info.Metadata == nil || info.Metadata.Name != "Newer" {
		t.Fatalf("not the newer registry: %+v", info.Metadata)
	}
}

// TestManagerOneReadPerToken ensures a cycle while a token's read is under way
// doesn't read it again.
func TestManagerOneReadPerToken(t *testing.T) {
	category := chainhash.Hash{1}
	registry := registryFor(category, "Token", "TOK")
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	m, _ := readsFixture(t, map[chainhash.Hash][]byte{category: registry}, func(chainhash.Hash) {
		arrived <- struct{}{}
		<-release
	})
	m.Tokens([]chainhash.Hash{category})
	for range 2 {
		m.lastFull = time.Time{}
		if err := m.cycle(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	<-arrived
	if len(m.flights) != 1 {
		t.Fatalf("%d downloads under way", len(m.flights))
	}
	for _, reads := range m.flights {
		if len(reads) != 1 {
			t.Fatalf("%d reads of the token under way", len(reads))
		}
	}

	// The read ends before the test, which would remove the cache under it.
	close(release)
	if err := waitReads(m); err != nil {
		t.Fatal(err)
	}
}

// TestManagerRetryOutlastsRestart ensures a registry that couldn't be read waits
// out its retry after a restart too, and that a Refresh still reads it at once.
func TestManagerRetryOutlastsRestart(t *testing.T) {
	category := chainhash.Hash{1}
	f := newManagerFixture(t, category, registryFor(category, "Token", "TOK"), nil, always)
	f.backend.pubs[category].Hash = [32]byte{1} // Nothing will match it.
	f.m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(f.m); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 1 {
		t.Fatalf("%d downloads", f.hits.Load())
	}

	m, err := NewManager(f.m.cfg)
	if err != nil {
		t.Fatal(err)
	}
	m.Tokens([]chainhash.Hash{category})
	if err := cycleAndRead(m); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 1 {
		t.Fatalf("%d downloads after a restart", f.hits.Load())
	}
	if failed := m.tried[retryKey{category, [32]byte{1}}]; failed.tries != 1 {
		t.Fatalf("%+v", failed)
	}

	if err := m.Refresh(category); err != nil {
		t.Fatal(err)
	}
	if err := cycleAndRead(m); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 2 {
		t.Fatalf("%d downloads after a refresh", f.hits.Load())
	}
	if m.tried[retryKey{category, [32]byte{1}}].tries != 1 {
		t.Fatalf("a refresh didn't start the count again: %+v", m.tried)
	}
}

// TestManagerStrangerSharesNoWait ensures a token whose registry hash another
// publishes with URIs of its own, which keep their reader waiting, reads its own
// URIs meanwhile, and that the file it gets serves the other too.
func TestManagerStrangerSharesNoWait(t *testing.T) {
	stranger, victim := chainhash.Hash{1}, chainhash.Hash{2}
	shared := sharedRegistry(stranger, victim)
	release := make(chan struct{})
	defer close(release)
	m, _ := readsFixture(t, map[chainhash.Hash][]byte{stranger: shared, victim: shared},
		func(category chainhash.Hash) {
			if category == stranger {
				<-release
			}
		})
	m.Tokens([]chainhash.Hash{stranger, victim})
	if err := m.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute)
	for m.Peek([]chainhash.Hash{victim})[0].Metadata == nil {
		if time.Now().After(deadline) {
			t.Fatal("the stranger's URIs held up the token")
		}
		select {
		case read := <-m.results:
			m.finishRead(context.Background(), read)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := waitReads(m); err != nil {
		t.Fatal(err)
	}
	if info := m.Peek([]chainhash.Hash{stranger})[0]; info.Metadata == nil || info.Metadata.Name != "One" {
		t.Fatalf("the file didn't serve the stranger: %+v", info.Metadata)
	}
}

// TestManagerRefreshWhileOut ensures a Refresh says at once that every gateway
// of the token's registry is out, and that the token is still read later, and
// that it says nothing while one is in rotation.
func TestManagerRefreshWhileOut(t *testing.T) {
	category := chainhash.Hash{1}
	f := newManagerFixture(t, category, registryFor(category, "Token", "TOK"), nil, always)
	f.m.cfg.Fetcher.Gateways = []string{"https://gw1.example", "https://gw2.example"}
	f.backend.pubs[category].URIs = []string{"ipfs://bafkrei1"}
	f.m.Tokens([]chainhash.Hash{category})
	f.m.lastFull = time.Time{}
	if err := f.m.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}

	fetcher := f.m.cfg.Fetcher
	fetcher.healthMu.Lock()
	fetcher.out["gw1.example"] = gatewayOut{failures: 1, until: time.Now().Add(time.Hour)}
	fetcher.healthMu.Unlock()
	if err := f.m.Refresh(category); err != nil {
		t.Fatalf("a refresh with a gateway left: %v", err)
	}
	fetcher.healthMu.Lock()
	fetcher.out["gw2.example"] = gatewayOut{failures: 1, until: time.Now().Add(time.Hour)}
	fetcher.healthMu.Unlock()
	if err := f.m.Refresh(category); !errors.Is(err, ErrNoGateway) {
		t.Fatalf("a refresh with every gateway out: %v", err)
	}
	f.m.mu.Lock()
	refresh, wanted := f.m.wanted[category]
	f.m.mu.Unlock()
	if !wanted || !refresh {
		t.Fatal("the token isn't read later")
	}
}
