package bcmr

import (
	"bytes"
	"encoding/gob"
	"path/filepath"
	"reflect"
	"testing"

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
	db := testDB(t)
	store, err := NewStore(db)
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
	store, err = NewStore(db)
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

// storedTypes returns what the types bucket holds for a state.
func storedTypes(t *testing.T, db walletdb.DB, category chainhash.Hash) []byte {
	t.Helper()
	var stored []byte
	err := walletdb.View(db, func(tx walletdb.ReadTx) error {
		stored = append(stored, tx.ReadBucket(bcmrNFTsBucket).Get(category[:])...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

// TestStoreKeepsTypesApart ensures a state's NFT types come back, and that a save
// for a new block doesn't write them.
func TestStoreKeepsTypesApart(t *testing.T) {
	db := testDB(t)
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	state := typesState()
	state.Category = chainhash.Hash{1}
	if err := store.Put(state); err != nil {
		t.Fatal(err)
	}
	first := storedTypes(t, db, state.Category)
	if len(first) == 0 {
		t.Fatal("no types stored")
	}

	// Writing other bytes under the key shows whether the next save writes them.
	mark := []byte("unchanged")
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		return tx.ReadWriteBucket(bcmrNFTsBucket).Put(state.Category[:], mark)
	})
	if err != nil {
		t.Fatal(err)
	}
	state.ScannedTo = 100
	if err := store.Put(state); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(storedTypes(t, db, state.Category), mark) {
		t.Fatal("a new block wrote the types")
	}

	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		return tx.ReadWriteBucket(bcmrNFTsBucket).Put(state.Category[:], first)
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err = NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	all, err := store.All()
	if err != nil {
		t.Fatal(err)
	}
	got := all[state.Category]
	if got == nil || got.ScannedTo != 100 || !reflect.DeepEqual(got.Metadata.NFTTypes, state.Metadata.NFTTypes) {
		t.Fatalf("%+v", got)
	}
}

// TestStoreReadsTypesKeptWithTheState ensures a state saved before the types were
// kept apart keeps them, and moves them at its next save.
func TestStoreReadsTypesKeptWithTheState(t *testing.T) {
	db := testDB(t)
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	state := typesState()
	state.Category = chainhash.Hash{2}
	var old bytes.Buffer
	if err := gob.NewEncoder(&old).Encode(state); err != nil {
		t.Fatal(err)
	}
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		return tx.ReadWriteBucket(bcmrBucket).Put(state.Category[:], old.Bytes())
	})
	if err != nil {
		t.Fatal(err)
	}
	all, err := store.All()
	if err != nil {
		t.Fatal(err)
	}
	got := all[state.Category]
	if got == nil || len(got.Metadata.NFTTypes) != 50 {
		t.Fatalf("%+v", got)
	}
	if err := store.Put(got); err != nil {
		t.Fatal(err)
	}
	if len(storedTypes(t, db, state.Category)) == 0 {
		t.Fatal("the types weren't moved")
	}
}

func typesState() *State {
	types := make(map[string]NFTType)
	for i := range 50 {
		types[string(rune('a'+i))] = NFTType{Name: "N"}
	}
	return &State{Metadata: &Metadata{Name: "T", NFTTypes: types}}
}
