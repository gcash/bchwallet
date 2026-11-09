package bcmr

import (
	"bytes"
	"encoding/gob"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchwallet/walletdb"
	_ "github.com/gcash/bchwallet/walletdb/bdb"
)

// testDB returns an empty wallet database.
func testDB(t *testing.T) walletdb.DB {
	t.Helper()
	db, err := walletdb.Create("bdb", filepath.Join(t.TempDir(), "wallet.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestStoreRoundTrip ensures a state comes back as it was stored, and that
// opening the store again keeps it.
func TestStoreRoundTrip(t *testing.T) {
	db, dir := testDB(t), t.TempDir()
	store, err := NewStore(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	older := chainhash.Hash{5}
	want := &State{
		Category: chainhash.Hash{1},
		Links: []Link{{Txid: chainhash.Hash{2}, Height: 10, BlockHash: chainhash.Hash{3},
			Publication: &Publication{Hash: [32]byte{4}, URIs: []string{"example.com"}}}},
		OlderTxid:    &older,
		OlderHeight:  9,
		Burned:       true,
		ScannedTo:    11,
		ScannedHash:  chainhash.Hash{6},
		Status:       StatusVerified,
		RegistryHash: [32]byte{7},
		Metadata:     &Metadata{Name: "T", Symbol: "T", Decimals: 2},
	}
	if err := store.Put(want); err != nil {
		t.Fatal(err)
	}
	store, err = NewStore(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	all, err := store.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || !reflect.DeepEqual(all[want.Category], want) {
		t.Fatalf("got %+v", all[want.Category])
	}
}

// TestStateHeadAndPublication ensures the head is the last link, and that the
// newest publication wins, with the older one last.
func TestStateHeadAndPublication(t *testing.T) {
	older := &Publication{URIs: []string{"older"}}
	newer := &Publication{URIs: []string{"newer"}}
	state := &State{Older: older}
	if state.Head() != nil || state.CurrentPublication() != older {
		t.Fatal("with no links")
	}
	state.Links = []Link{{Height: 1}, {Height: 2, Publication: newer}, {Height: 3}}
	if state.Head().Height != 3 || state.CurrentPublication() != newer {
		t.Fatalf("head %+v", state.Head())
	}
	state.Links = state.Links[:1]
	if state.CurrentPublication() != older {
		t.Fatal("the older publication wasn't used")
	}
}

// storedTypes returns what the types file holds for a state.
func storedTypes(t *testing.T, dir string, category chainhash.Hash) []byte {
	t.Helper()
	stored, err := os.ReadFile(filepath.Join(dir, category.String()+".gob"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return stored
}

// TestStoreKeepsTypesApart ensures a state's NFT types come back, and that a save
// for a new block doesn't write them.
func TestStoreKeepsTypesApart(t *testing.T) {
	db, dir := testDB(t), t.TempDir()
	store, err := NewStore(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	state := typesState()
	state.Category = chainhash.Hash{1}
	if err := store.Put(state); err != nil {
		t.Fatal(err)
	}
	first := storedTypes(t, dir, state.Category)
	if len(first) == 0 {
		t.Fatal("no types stored")
	}

	// Writing other bytes under the key shows whether the next save writes them.
	mark := []byte("unchanged")
	if err := os.WriteFile(filepath.Join(dir, state.Category.String()+".gob"), mark, 0600); err != nil {
		t.Fatal(err)
	}
	state.ScannedTo = 100
	if err := store.Put(state); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(storedTypes(t, dir, state.Category), mark) {
		t.Fatal("a new block wrote the types")
	}

	if err := os.WriteFile(filepath.Join(dir, state.Category.String()+".gob"), first, 0600); err != nil {
		t.Fatal(err)
	}
	store, err = NewStore(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	all, err := store.All()
	if err != nil {
		t.Fatal(err)
	}
	got := all[state.Category]
	if got == nil || got.ScannedTo != 100 || got.Metadata.NFTTypes != nil ||
		!reflect.DeepEqual(store.NFTTypes(state.Category), state.Metadata.NFTTypes) {

		t.Fatalf("%+v", got)
	}
}

// TestStoreReadsTypesKeptWithTheState ensures a state saved before the types were
// kept apart keeps them, and moves them at its next save.
func TestStoreReadsTypesKeptWithTheState(t *testing.T) {
	db, dir := testDB(t), t.TempDir()
	store, err := NewStore(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	state := oldTypesState(t, db, chainhash.Hash{2})
	all, err := store.All()
	if err != nil {
		t.Fatal(err)
	}
	got := all[state.Category]
	if got == nil || got.Metadata.NFTTypes != nil || got.Metadata.NFTTypeCount != 50 ||
		got.RegistryParser != registryParser || len(store.NFTTypes(state.Category)) != 50 {

		t.Fatalf("%+v", got)
	}
	if len(storedTypes(t, dir, state.Category)) == 0 {
		t.Fatal("the types weren't moved")
	}
}

// TestStoreTypesThatCantMove ensures types kept with a state that can't go to
// their file have the registry read again.
func TestStoreTypesThatCantMove(t *testing.T) {
	db, dir := testDB(t), t.TempDir()
	store, err := NewStore(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	state := oldTypesState(t, db, chainhash.Hash{7})
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0700) }()
	all, err := store.All()
	if err != nil {
		t.Fatal(err)
	}
	if got := all[state.Category]; got == nil || got.RegistryParser != 0 {
		t.Fatalf("%+v", got)
	}
}

// oldTypesState stores a state as the store did before the types were kept
// apart, with them and without their number.
func oldTypesState(t *testing.T, db walletdb.DB, category chainhash.Hash) *State {
	t.Helper()
	state := typesState()
	state.Category = category
	state.RegistryParser = registryParser
	state.Metadata.NFTTypeCount = 0
	var old bytes.Buffer
	if err := gob.NewEncoder(&old).Encode(state); err != nil {
		t.Fatal(err)
	}
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		return tx.ReadWriteBucket(bcmrBucket).Put(state.Category[:], old.Bytes())
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// TestStoreKeepsRecentTypes ensures the types read last are kept, the newest
// first, up to maxRecentTypes, and that types that change are read anew.
func TestStoreKeepsRecentTypes(t *testing.T) {
	db, dir := testDB(t), t.TempDir()
	store, err := NewStore(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	states := make([]*State, maxRecentTypes+1)
	for i := range states {
		states[i] = typesState()
		states[i].Category = chainhash.Hash{0x40, byte(i)}
		if err := store.Put(states[i]); err != nil {
			t.Fatal(err)
		}
	}
	remove := func(state *State) {
		t.Helper()
		if err := os.Remove(filepath.Join(dir, state.Category.String()+".gob")); err != nil {
			t.Fatal(err)
		}
	}

	// Reading the first again, after the others, keeps it the newest, so
	// the second is the one that goes when the last is read.
	for _, state := range states[:maxRecentTypes] {
		store.NFTTypes(state.Category)
	}
	store.NFTTypes(states[0].Category)
	store.NFTTypes(states[maxRecentTypes].Category)
	remove(states[0])
	remove(states[1])
	remove(states[2])
	if len(store.NFTTypes(states[0].Category)) != 50 || len(store.NFTTypes(states[2].Category)) != 50 {
		t.Fatal("the types read last weren't kept")
	}
	if store.NFTTypes(states[1].Category) != nil {
		t.Fatal("more types were kept than maxRecentTypes")
	}

	// Types that change are read anew.
	state := states[3]
	state.RegistryHash = [32]byte{1}
	state.Metadata.NFTTypes = map[string]NFTType{"a": {Name: "New"}}
	state.Metadata.NFTTypeCount = 1
	if err := store.Put(state); err != nil {
		t.Fatal(err)
	}
	if got := store.NFTTypes(state.Category); len(got) != 1 || got["a"].Name != "New" {
		t.Fatalf("%+v", got)
	}
}

func typesState() *State {
	types := make(map[string]NFTType)
	for i := range 50 {
		types[string(rune('a'+i))] = NFTType{Name: "N"}
	}
	return &State{Metadata: &Metadata{Name: "T", NFTTypes: types, NFTTypeCount: len(types)}}
}

// TestStoreSnapshotTakesOver ensures the types of a snapshot that took over are
// written, though the registry and the number of types are the same.
func TestStoreSnapshotTakesOver(t *testing.T) {
	db, dir := testDB(t), t.TempDir()
	store, err := NewStore(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	state := typesState()
	state.Category = chainhash.Hash{3}
	state.Metadata.NextSnapshot = time.Unix(1000, 0)
	if err := store.Put(state); err != nil {
		t.Fatal(err)
	}
	revealed := *state.Metadata
	revealed.NextSnapshot = time.Time{}
	revealed.NFTTypes = make(map[string]NFTType)
	for name := range state.Metadata.NFTTypes {
		revealed.NFTTypes[name] = NFTType{Name: "Revealed"}
	}
	state.Metadata = &revealed
	if err := store.Put(state); err != nil {
		t.Fatal(err)
	}
	store, err = NewStore(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.NFTTypes(state.Category)["a"]; got.Name != "Revealed" {
		t.Fatalf("%+v", got)
	}
}

// TestStoreMovesTypesOutOfTheDatabase ensures the NFT types kept in the wallet's
// database are moved to their files when the store opens, and the bucket goes.
func TestStoreMovesTypesOutOfTheDatabase(t *testing.T) {
	db, dir := testDB(t), t.TempDir()
	state := typesState()
	state.Category = chainhash.Hash{4}
	light := *state
	metadata := *state.Metadata
	metadata.NFTTypes = nil
	light.Metadata = &metadata
	var value, types bytes.Buffer
	if err := gob.NewEncoder(&value).Encode(&light); err != nil {
		t.Fatal(err)
	}
	if err := gob.NewEncoder(&types).Encode(state.Metadata.NFTTypes); err != nil {
		t.Fatal(err)
	}
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		for name, value := range map[string][]byte{string(bcmrBucket): value.Bytes(),
			string(bcmrNFTsBucket): types.Bytes()} {

			bucket, err := tx.CreateTopLevelBucket([]byte(name))
			if err != nil {
				return err
			}
			if err := bucket.Put(state.Category[:], value); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	err = walletdb.View(db, func(tx walletdb.ReadTx) error {
		if tx.ReadBucket(bcmrNFTsBucket) != nil {
			return errors.New("the bucket of types is still there")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	all, err := store.All()
	if err != nil {
		t.Fatal(err)
	}
	if got := all[state.Category]; got == nil || len(store.NFTTypes(state.Category)) != 50 {
		t.Fatalf("%+v", got)
	}
}

// TestStoreUnreadableTypes ensures a types file that can't be read has the
// registry read again.
func TestStoreUnreadableTypes(t *testing.T) {
	db, dir := testDB(t), t.TempDir()
	store, err := NewStore(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	state := typesState()
	state.Category = chainhash.Hash{5}
	state.RegistryParser = registryParser
	if err := store.Put(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, state.Category.String()+".gob"), []byte("torn"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err = NewStore(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	if types := store.NFTTypes(state.Category); types != nil {
		t.Fatalf("read a torn file: %v", types)
	}
	all, err := store.All()
	if err != nil {
		t.Fatal(err)
	}
	if got := all[state.Category]; got.RegistryParser != 0 {
		t.Fatalf("%+v", got)
	}
}

// TestStoreRemovesTypes ensures a state whose metadata has no NFT types any more
// leaves no types file, and comes back without them.
func TestStoreRemovesTypes(t *testing.T) {
	db, dir := testDB(t), t.TempDir()
	store, err := NewStore(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	state := typesState()
	state.Category = chainhash.Hash{6}
	if err := store.Put(state); err != nil {
		t.Fatal(err)
	}
	state.Metadata = &Metadata{Name: "T"}
	state.RegistryHash = [32]byte{1}
	if err := store.Put(state); err != nil {
		t.Fatal(err)
	}
	if len(storedTypes(t, dir, state.Category)) != 0 {
		t.Fatal("the types file is still there")
	}

	// Nor does metadata that has none, from the same registry.
	other := typesState()
	other.Category = chainhash.Hash{6, 1}
	if err := store.Put(other); err != nil {
		t.Fatal(err)
	}
	other.Metadata = &Metadata{Name: "T"}
	if err := store.Put(other); err != nil {
		t.Fatal(err)
	}
	if len(storedTypes(t, dir, other.Category)) != 0 {
		t.Fatal("the types of metadata without any are still there")
	}
	all, err := store.All()
	if err != nil {
		t.Fatal(err)
	}
	if got := all[state.Category]; got.Metadata.NFTTypes != nil || store.NFTTypes(state.Category) != nil {
		t.Fatalf("%+v", got.Metadata)
	}
}
