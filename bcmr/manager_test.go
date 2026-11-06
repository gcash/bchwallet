package bcmr

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	if err := f.m.cycle(context.Background()); err != nil {
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
	if err := f.m.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.backend.preparedCount() != 0 || f.hits.Load() != 0 {
		t.Fatalf("%d prepared, %d downloads", f.backend.preparedCount(), f.hits.Load())
	}

	f.m.Refresh(category)
	if err := f.m.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	info := f.m.Tokens([]chainhash.Hash{category})[0]
	if info.Metadata == nil || info.Status != StatusVerified {
		t.Fatalf("got %+v", info)
	}

	// Once asked for, it's followed from then on.
	if err := f.m.cycle(context.Background()); err != nil {
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
		during = f.m.Tokens([]chainhash.Hash{category})[0]
		states[0].Status = StatusVerified
		save(states[0])
		return nil
	}
	f.m.Tokens([]chainhash.Hash{category})
	if err := f.m.cycle(context.Background()); err != nil {
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
		if err := f.m.cycle(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if f.hits.Load() != 1 {
		t.Fatalf("%d downloads", f.hits.Load())
	}
	f.m.Refresh(category)
	if err := f.m.cycle(context.Background()); err != nil {
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
		if err := f.m.cycle(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if f.backend.preparedCount() != 1 {
		t.Fatalf("%d prepared", f.backend.preparedCount())
	}
	tip = chainhash.Hash{2}
	if err := f.m.cycle(context.Background()); err != nil {
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
	if err := f.m.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(f.m.cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.backend.prepared = nil
	if err := m.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.backend.preparedCount() != 0 {
		t.Fatal("a token that isn't shown was followed")
	}
	m.Tokens([]chainhash.Hash{category})
	if err := m.cycle(context.Background()); err != nil {
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
	if err := f.m.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := f.m.states[category]
	state.Links = []Link{{Height: 1, Publication: &Publication{Hash: [32]byte{1},
		URIs: []string{f.url}}}}
	f.m.fetchRegistries(context.Background(), []*State{state})
	if info := f.m.views[category]; info.Metadata != nil {
		t.Fatalf("got %+v", info)
	}

	f.backend.pubs = nil
	state.Metadata, state.RegistryHash = &Metadata{Name: "Old"}, [32]byte{9}
	state.Links = []Link{{Height: 1}}
	f.m.fetchRegistries(context.Background(), []*State{state})
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
		if err := f.m.cycle(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if f.hits.Load() != 1 {
		t.Fatalf("%d downloads", f.hits.Load())
	}
	f.m.Refresh(category)
	if err := f.m.cycle(context.Background()); err != nil {
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
		if err := f.m.cycle(context.Background()); err != nil {
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
	if err := f.m.cycle(context.Background()); err != nil {
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
		if err := f.m.cycle(context.Background()); err != nil {
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
	if err := f.m.cycle(context.Background()); err != nil {
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
	if err := m.cycle(context.Background()); err != nil {
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
	if err := f.m.cycle(context.Background()); err != nil {
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
	if err := f.m.cycle(context.Background()); err == nil {
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
	if err := f.m.cycle(context.Background()); err != nil {
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
	f.m.fetchRegistries(context.Background(), []*State{state})
	if f.hits.Load() != hits {
		t.Fatal("read again right away")
	}
	state.RegistryRead = time.Now().Add(-changedRegistryInterval - time.Minute)
	f.m.fetchRegistries(context.Background(), []*State{state})
	if f.hits.Load() != hits+1 {
		t.Fatal("not read again after the interval")
	}

	f.m.Refresh(category)
	f.m.takeWanted()
	if state.Metadata == nil {
		t.Fatal("Refresh dropped what shows, which no cache can bring back")
	}
	f.m.fetchRegistries(context.Background(), []*State{state})
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
	if err := f.m.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if info := f.m.Tokens([]chainhash.Hash{category})[0]; !info.RegistryFailed {
		t.Fatalf("%+v", info)
	}
	state := f.m.states[category]
	for i := range state.Links {
		state.Links[i].Publication.URIs = uris
	}
	f.m.Refresh(category)
	f.m.takeWanted()
	f.m.fetchRegistries(context.Background(), []*State{state})
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
	if err := f.m.cycle(context.Background()); err != nil {
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
	if err := f.m.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := f.m.states[category]
	for i := range state.Links {
		state.Links[i].Publication.URIs = []string{"https://127.0.0.1:1/r"}
	}
	f.m.Refresh(category)
	f.m.takeWanted()
	f.m.fetchRegistries(context.Background(), []*State{state})
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
	if err := f.m.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := f.m.states[category]
	state.Metadata = nil
	f.m.fetchRegistries(context.Background(), []*State{state})
	if info := f.m.Tokens([]chainhash.Hash{category})[0]; !info.RegistryFailed {
		t.Fatalf("%+v", info)
	}
}

// TestManagerNewParser ensures a registry read by an older parser is read again.
func TestManagerNewParser(t *testing.T) {
	category := chainhash.Hash{0x37}
	f := newManagerFixture(t, category, registryFor(category, "Coin", "COIN"), nil, always)
	f.m.Tokens([]chainhash.Hash{category})
	if err := f.m.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := f.m.states[category]
	hits := f.hits.Load()
	state.RegistryParser = 0
	state.Metadata.Name = "Old"
	f.m.fetchRegistries(context.Background(), []*State{state})
	if f.hits.Load() != hits+1 || state.Metadata.Name != "Coin" || state.RegistryParser != registryParser {
		t.Fatalf("%d hits, %+v", f.hits.Load()-hits, state.Metadata)
	}
	f.m.fetchRegistries(context.Background(), []*State{state})
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
	if err := f.m.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := f.m.states[category]
	state.RegistryHash = f.backend.pubs[category].Hash
	state.Metadata = &Metadata{Name: "Shown", Symbol: "S"}
	state.RegistryFailed, state.RegistryParser = false, 0
	f.m.fetchRegistries(context.Background(), []*State{state})
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
	if err := f.m.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := f.m.states[category]
	state.Links[0].Publication.Hash = sha256.Sum256([]byte("published"))
	state.RegistryHash = state.Links[0].Publication.Hash
	state.Metadata = &Metadata{Name: "Published", Symbol: "P", Decimals: 8}
	state.RegistryParser = 0
	f.m.fetchRegistries(context.Background(), []*State{state})
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
	if err := f.m.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := f.m.states[category]
	hits := f.hits.Load()
	state.Metadata.NextSnapshot = time.Now().Add(time.Hour)
	f.m.fetchRegistries(context.Background(), []*State{state})
	if f.hits.Load() != hits {
		t.Fatal("read before its time")
	}
	state.Metadata.NextSnapshot = time.Now().Add(-time.Hour)
	f.m.fetchRegistries(context.Background(), []*State{state})
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
	if err := f.m.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := f.m.states[category]
	hits := f.hits.Load()
	f.m.fetchRegistries(context.Background(), []*State{state})
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
	if err := f.m.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != 0 || len(f.m.asked) != 0 {
		t.Fatal("Peek had the token load")
	}

	f.m.Tokens([]chainhash.Hash{category})
	if err := f.m.cycle(context.Background()); err != nil {
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
	if err := f.m.cycle(context.Background()); err != nil {
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
	if err := m.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.hits.Load() != hits || len(m.asked) != 0 {
		t.Fatalf("%d more reads", f.hits.Load()-hits)
	}
}
