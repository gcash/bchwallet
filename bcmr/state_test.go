package bcmr

import (
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

// TestStoreSkipsUnchangedTypes ensures a state with NFT types costs no write when
// it didn't change, though gob writes a map in a random order.
func TestStoreSkipsUnchangedTypes(t *testing.T) {
	first, err := digest(typesState())
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		next, err := digest(typesState())
		if err != nil {
			t.Fatal(err)
		}
		if next != first {
			t.Fatal("the digest of an unchanged state changed")
		}
	}
}

func typesState() *State {
	types := make(map[string]NFTType)
	for i := range 50 {
		types[string(rune('a'+i))] = NFTType{Name: "N"}
	}
	return &State{Metadata: &Metadata{Name: "T", NFTTypes: types}}
}
