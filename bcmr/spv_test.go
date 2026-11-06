package bcmr

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	chainbcmr "github.com/gcash/bchd/bcmr"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
)

// fakeChain is a chain of blocks, from height 1, that searches them directly.
type fakeChain struct {
	// duringSearch is told of each block a search has read.
	duringSearch func(height int32)

	blocks   []*wire.MsgBlock
	searches [][2]int32
	failAt   int32

	failBlock bool
}

func newFakeChain(height int) *fakeChain {
	c := &fakeChain{}
	for h := 1; h <= height; h++ {
		c.addBlock(int32(h))
	}
	return c
}

func (c *fakeChain) addBlock(nonce int32, txs ...*wire.MsgTx) {
	c.blocks = append(c.blocks, &wire.MsgBlock{
		Header:       wire.BlockHeader{Nonce: uint32(nonce)},
		Transactions: txs,
	})
}

// put adds a transaction to the block at a height.
func (c *fakeChain) put(height int32, txs ...*wire.MsgTx) {
	block := c.blocks[height-1]
	block.Transactions = append(block.Transactions, txs...)
}

// replaceFrom takes the chain back to below height, with new blocks to its old
// height, as a reorganization does.
func (c *fakeChain) replaceFrom(height int32, salt int32) {
	end := int32(len(c.blocks))
	c.blocks = c.blocks[:height-1]
	for h := height; h <= end; h++ {
		c.addBlock(h + salt)
	}
}

func (c *fakeChain) GetBestBlock() (*chainhash.Hash, int32, error) {
	height := int32(len(c.blocks))
	hash, err := c.GetBlockHash(int64(height))
	return hash, height, err
}

func (c *fakeChain) GetBlockHash(height int64) (*chainhash.Hash, error) {
	if height < 1 || height > int64(len(c.blocks)) {
		return nil, fmt.Errorf("no block at %d", height)
	}
	hash := c.blocks[height-1].BlockHash()
	return &hash, nil
}

func (c *fakeChain) GetBlock(hash *chainhash.Hash) (*wire.MsgBlock, error) {
	if c.failBlock {
		return nil, errors.New("no peer sent the block")
	}
	for _, block := range c.blocks {
		if block.BlockHash() == *hash {
			return block, nil
		}
	}
	return nil, errors.New("no such block")
}

func (c *fakeChain) SearchSpends(outpoints []wire.OutPoint, from, to int32,
	found func(SpendFound) []wire.OutPoint, progress func(int32),
	quit <-chan struct{}) (int32, error) {

	c.searches = append(c.searches, [2]int32{from, to})
	step := int32(1)
	if from > to {
		step = -1
	}
	watched := make(map[wire.OutPoint]bool)
	for _, outpoint := range outpoints {
		watched[outpoint] = true
	}
	reached := from - step
	for h := from; h != to+step && len(watched) > 0; h += step {
		if h == c.failAt {
			return reached, errors.New("no peer sent the filter")
		}
		c.searchBlock(h, watched, found, step > 0)
		reached = h
		if c.duringSearch != nil {
			c.duringSearch(h)
		}
		if progress != nil {
			progress(h)
		}
	}
	return reached, nil
}

// searchBlock reads a block as neutrino does: every input is indexed, then
// what's watched is reported once each, in block order and then along chains.
func (c *fakeChain) searchBlock(h int32, watched map[wire.OutPoint]bool,
	found func(SpendFound) []wire.OutPoint, follow bool) {

	block := c.blocks[h-1]
	spends := make(map[wire.OutPoint]*wire.MsgTx)
	reported := make(map[wire.OutPoint]bool)
	var pending []wire.OutPoint
	for _, tx := range block.Transactions {
		for _, in := range tx.TxIn {
			spends[in.PreviousOutPoint] = tx
			if watched[in.PreviousOutPoint] {
				pending = append(pending, in.PreviousOutPoint)
			}
		}
	}
	for len(pending) > 0 {
		outpoint := pending[0]
		pending = pending[1:]
		delete(watched, outpoint)
		reported[outpoint] = true
		next := found(SpendFound{outpoint, spends[outpoint], h, block.BlockHash()})
		if !follow {
			continue
		}
		for _, n := range next {
			_, inBlock := spends[n]
			if watched[n] || (inBlock && reported[n]) {
				continue
			}
			watched[n] = true
			if inBlock {
				pending = append(pending, n)
			}
		}
	}
}

// spendOf returns a transaction that spends output 0 of parent, with an
// ordinary output 0.
func spendOf(id int64, parent chainhash.Hash) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: parent}, nil))
	tx.AddTxOut(wire.NewTxOut(id, []byte{txscript.OP_TRUE}, wire.TokenData{}))
	return tx
}

// withPublication adds a publication to tx.
func withPublication(t *testing.T, tx *wire.MsgTx, hashByte byte, uri string) *wire.MsgTx {
	t.Helper()
	hash := [32]byte{hashByte}
	script, err := txscript.NewScriptBuilder().AddOp(txscript.OP_RETURN).
		AddData([]byte("BCMR")).AddData(hash[:]).AddData([]byte(uri)).Script()
	if err != nil {
		t.Fatal(err)
	}
	tx.AddTxOut(wire.NewTxOut(0, script, wire.TokenData{}))
	return tx
}

// genesisOf returns a transaction that creates the category.
func genesisOf(id int64, category chainhash.Hash) *wire.MsgTx {
	tx := spendOf(id, category)
	var categoryID [32]byte
	copy(categoryID[:], category[:])
	tx.TxOut[0].TokenData = wire.TokenData{CategoryID: categoryID, Amount: 100, BitField: 0x10}
	return tx
}

// burnOf returns a transaction that spends output 0 of parent to a data output.
func burnOf(parent chainhash.Hash) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: parent}, nil))
	tx.AddTxOut(wire.NewTxOut(0, []byte{txscript.OP_RETURN}, wire.TokenData{}))
	return tx
}

// hintsFor returns a hints table of the entries at a height.
func hintsFor(t *testing.T, height int32, hints ...chainbcmr.Hint) *chainbcmr.Hints {
	t.Helper()
	parsed, err := chainbcmr.ParseHints(chainbcmr.EncodeHints(height, hints))
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func noSave(*State) {}

// resolve runs both steps.
func resolve(t *testing.T, b *SPVBackend, states ...*State) {
	t.Helper()
	if err := b.Prepare(context.Background(), states); err != nil {
		t.Fatal(err)
	}
	if err := b.Advance(context.Background(), states, noSave); err != nil {
		t.Fatal(err)
	}
}

// TestSPVFromHint ensures a token in the table starts there, reads no filter
// below its height, and follows the links above it.
func TestSPVFromHint(t *testing.T) {
	c := newFakeChain(100)
	category := chainhash.Hash{1}
	head := withPublication(t, spendOf(1, chainhash.Hash{2}), 7, "head.example")
	next := spendOf(2, head.TxHash())
	last := withPublication(t, spendOf(3, next.TxHash()), 8, "last.example")
	c.put(10, head)
	c.put(60, next)
	c.put(70, last)

	hints := hintsFor(t, 50, chainbcmr.Hint{Category: category, Head: head.TxHash(), HeadHeight: 10})
	b := NewSPVBackend(c, hints)
	state := &State{Category: category}
	resolve(t, b, state)

	if !reflect.DeepEqual(c.searches, [][2]int32{{51, 100}}) {
		t.Fatalf("searches %v", c.searches)
	}
	if state.Head().Txid != last.TxHash() || len(state.Links) != 3 {
		t.Fatalf("head %+v, %d links", state.Head(), len(state.Links))
	}
	if state.Status != StatusVerified || state.ScannedTo != 100 {
		t.Fatalf("status %v, scanned to %d", state.Status, state.ScannedTo)
	}
	if pub := state.CurrentPublication(); pub == nil || pub.URIs[0] != "last.example" {
		t.Fatalf("publication %+v", pub)
	}
}

// TestSPVPrepareShowsWhatsThere ensures the table's publication is there after
// Prepare, before any scan, so it can show while the scan runs.
func TestSPVPrepareShowsWhatsThere(t *testing.T) {
	c := newFakeChain(100)
	category := chainhash.Hash{1}
	older := withPublication(t, spendOf(1, chainhash.Hash{2}), 7, "older.example")
	head := spendOf(2, older.TxHash())
	c.put(10, older)
	c.put(20, head)
	hints := hintsFor(t, 50, chainbcmr.Hint{Category: category, Head: head.TxHash(), HeadHeight: 20,
		Publication: &chainbcmr.HintPublication{Txid: older.TxHash(), Height: 10}})

	state := &State{Category: category}
	if err := NewSPVBackend(c, hints).Prepare(context.Background(), []*State{state}); err != nil {
		t.Fatal(err)
	}
	if len(c.searches) != 0 {
		t.Fatalf("searches %v", c.searches)
	}
	if state.Status != StatusSearching || state.ScannedTo != 50 {
		t.Fatalf("status %v, scanned to %d", state.Status, state.ScannedTo)
	}
	if pub := state.CurrentPublication(); pub == nil || pub.URIs[0] != "older.example" {
		t.Fatalf("publication %+v", pub)
	}
}

// TestSPVBurnedHint ensures a burned identity is never scanned.
func TestSPVBurnedHint(t *testing.T) {
	c := newFakeChain(100)
	head := burnOf(chainhash.Hash{2})
	c.put(10, head)
	hints := hintsFor(t, 50, chainbcmr.Hint{Category: chainhash.Hash{1}, Head: head.TxHash(),
		HeadHeight: 10, Burned: true})
	state := &State{Category: chainhash.Hash{1}}
	resolve(t, NewSPVBackend(c, hints), state)
	if len(c.searches) != 0 || !state.Burned || state.Status != StatusVerified {
		t.Fatalf("searches %v, state %+v", c.searches, state)
	}
}

// TestSPVBurnAboveHint ensures a burn found by a scan ends the chain.
func TestSPVBurnAboveHint(t *testing.T) {
	c := newFakeChain(100)
	head := spendOf(1, chainhash.Hash{2})
	burn := burnOf(head.TxHash())
	c.put(10, head)
	c.put(60, burn)
	hints := hintsFor(t, 50, chainbcmr.Hint{Category: chainhash.Hash{1}, Head: head.TxHash(), HeadHeight: 10})
	state := &State{Category: chainhash.Hash{1}}
	b := NewSPVBackend(c, hints)
	resolve(t, b, state)
	if !state.Burned || state.Head().Txid != burn.TxHash() || state.Status != StatusVerified {
		t.Fatalf("state %+v", state)
	}
	c.searches = nil
	if err := b.Advance(context.Background(), []*State{state}, noSave); err != nil {
		t.Fatal(err)
	}
	if len(c.searches) != 0 {
		t.Fatalf("a burned identity was scanned again: %v", c.searches)
	}
}

// TestSPVSearchNeverBelowHints ensures a token that isn't in the table is
// searched for down to just above the table's height and no further, and then
// followed up from its genesis.
func TestSPVSearchNeverBelowHints(t *testing.T) {
	c := newFakeChain(100)
	category := chainhash.Hash{1}
	genesis := genesisOf(1, category)
	next := spendOf(2, genesis.TxHash())
	c.put(80, genesis)
	c.put(90, next)
	state := &State{Category: category}
	resolve(t, NewSPVBackend(c, hintsFor(t, 50)), state)

	if !reflect.DeepEqual(c.searches, [][2]int32{{100, 51}, {80, 100}}) {
		t.Fatalf("searches %v", c.searches)
	}
	if state.Links[0].Txid != genesis.TxHash() || state.Head().Txid != next.TxHash() ||
		state.Status != StatusVerified {

		t.Fatalf("state %+v", state)
	}
}

// TestSPVMissingGenesis ensures a token that should be above the table's height
// and isn't fails rather than widening the search.
func TestSPVMissingGenesis(t *testing.T) {
	c := newFakeChain(100)
	category := chainhash.Hash{1}
	c.put(20, genesisOf(1, category))
	state := &State{Category: category}
	resolve(t, NewSPVBackend(c, hintsFor(t, 50)), state)
	if !reflect.DeepEqual(c.searches, [][2]int32{{100, 51}}) || state.Status != StatusFailed {
		t.Fatalf("searches %v, state %+v", c.searches, state)
	}
}

// TestSPVNotGenesis ensures a spend of the authbase that creates no such token
// isn't taken for its genesis.
func TestSPVNotGenesis(t *testing.T) {
	c := newFakeChain(100)
	category := chainhash.Hash{1}
	c.put(80, spendOf(1, category))
	state := &State{Category: category}
	resolve(t, NewSPVBackend(c, hintsFor(t, 50)), state)
	if state.Status != StatusFailed || len(state.Links) != 0 {
		t.Fatalf("state %+v", state)
	}
}

// TestSPVOnePass ensures several tokens share one pass, from the lowest height
// needed, and that two identities sharing a head both follow it.
func TestSPVOnePass(t *testing.T) {
	c := newFakeChain(100)
	a, b := chainhash.Hash{1}, chainhash.Hash{2}
	sharedHead := spendOf(1, chainhash.Hash{3})
	next := spendOf(2, sharedHead.TxHash())
	c.put(10, sharedHead)
	c.put(60, next)
	hints := hintsFor(t, 50,
		chainbcmr.Hint{Category: a, Head: sharedHead.TxHash(), HeadHeight: 10},
		chainbcmr.Hint{Category: b, Head: sharedHead.TxHash(), HeadHeight: 10})
	stateA, stateB := &State{Category: a}, &State{Category: b}
	resolve(t, NewSPVBackend(c, hints), stateA, stateB)
	if len(c.searches) != 1 {
		t.Fatalf("searches %v", c.searches)
	}
	if stateA.Head().Txid != next.TxHash() || stateB.Head().Txid != next.TxHash() {
		t.Fatalf("heads %v and %v", stateA.Head(), stateB.Head())
	}
}

// TestSPVMergeInOneBlock ensures chains that merge in a block where the links go
// on all get to the end, also the one that reaches the merge after the other
// has passed it.  This is what a real authchain on testnet4 does.
func TestSPVMergeInOneBlock(t *testing.T) {
	c := newFakeChain(100)
	shortCat, longCat := chainhash.Hash{1}, chainhash.Hash{2}
	longHead := spendOf(1, chainhash.Hash{3})
	shortHead := spendOf(2, chainhash.Hash{4})
	c.put(10, longHead, shortHead)

	// The long chain takes a hop more to the merge, whose input for the
	// short chain comes second, and the links go on past it.
	hop := spendOf(3, longHead.TxHash())
	merge := spendOf(4, hop.TxHash())
	merge.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: shortHead.TxHash()}, nil))
	after := spendOf(5, merge.TxHash())
	last := spendOf(6, after.TxHash())
	c.put(60, hop, merge, after, last)

	hints := hintsFor(t, 50,
		chainbcmr.Hint{Category: longCat, Head: longHead.TxHash(), HeadHeight: 10},
		chainbcmr.Hint{Category: shortCat, Head: shortHead.TxHash(), HeadHeight: 10})
	long, short := &State{Category: longCat}, &State{Category: shortCat}
	resolve(t, NewSPVBackend(c, hints), long, short)

	for _, state := range []*State{long, short} {
		if state.Head().Txid != last.TxHash() {
			t.Fatalf("token %v stopped at %v after %d links", state.Category,
				state.Head().Txid, len(state.Links))
		}
	}
}

// TestSPVMergeAfterPass ensures the shorter chain catches up when the longer one
// went past the merge before it was reached.
func TestSPVMergeAfterPass(t *testing.T) {
	c := newFakeChain(100)
	longHead := spendOf(1, chainhash.Hash{3})
	shortParent := spendOf(2, chainhash.Hash{4})
	c.put(10, longHead, shortParent)

	// The short chain's own hops come after the long chain is through.
	first := spendOf(3, shortParent.TxHash())
	second := spendOf(4, first.TxHash())
	merge := spendOf(5, longHead.TxHash())
	merge.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: second.TxHash()}, nil))
	after := spendOf(6, merge.TxHash())
	end := spendOf(7, after.TxHash())
	c.put(60, merge, after, end, first, second)

	hints := hintsFor(t, 50,
		chainbcmr.Hint{Category: chainhash.Hash{1}, Head: longHead.TxHash(), HeadHeight: 10},
		chainbcmr.Hint{Category: chainhash.Hash{2}, Head: shortParent.TxHash(), HeadHeight: 10})
	a, b := &State{Category: chainhash.Hash{1}}, &State{Category: chainhash.Hash{2}}
	resolve(t, NewSPVBackend(c, hints), a, b)
	if a.Head().Txid != end.TxHash() || b.Head().Txid != end.TxHash() {
		t.Fatalf("heads %v and %v", a.Head().Txid, b.Head().Txid)
	}
}

// TestSPVNextLinkInGenesisBlock ensures a link in the genesis's own block is
// followed, also with no other token to start the scan lower.
func TestSPVNextLinkInGenesisBlock(t *testing.T) {
	c := newFakeChain(100)
	category := chainhash.Hash{1}
	genesis := genesisOf(1, category)
	next := spendOf(2, genesis.TxHash())
	c.put(80, next, genesis)
	state := &State{Category: category}
	resolve(t, NewSPVBackend(c, hintsFor(t, 50)), state)
	if state.Head().Txid != next.TxHash() {
		t.Fatalf("head %v", state.Head().Txid)
	}
}

// TestSPVNewBlocks ensures a later pass only covers the new blocks, and picks up
// a head spent in them.
func TestSPVNewBlocks(t *testing.T) {
	c := newFakeChain(100)
	head := spendOf(1, chainhash.Hash{2})
	c.put(10, head)
	hints := hintsFor(t, 50, chainbcmr.Hint{Category: chainhash.Hash{1}, Head: head.TxHash(), HeadHeight: 10})
	state := &State{Category: chainhash.Hash{1}}
	b := NewSPVBackend(c, hints)
	resolve(t, b, state)

	next := spendOf(2, head.TxHash())
	c.addBlock(101)
	c.addBlock(102, next)
	c.searches = nil
	if err := b.Advance(context.Background(), []*State{state}, noSave); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.searches, [][2]int32{{101, 102}}) || state.Head().Txid != next.TxHash() {
		t.Fatalf("searches %v, head %+v", c.searches, state.Head())
	}

	// Nothing new costs nothing.
	c.searches = nil
	if err := b.Advance(context.Background(), []*State{state}, noSave); err != nil {
		t.Fatal(err)
	}
	if len(c.searches) != 0 {
		t.Fatalf("searches %v", c.searches)
	}
}

// TestSPVReorg ensures a link a reorganization took away is dropped, that the
// scan goes back, and that the state is no longer Verified until it's redone.
func TestSPVReorg(t *testing.T) {
	c := newFakeChain(100)
	head := spendOf(1, chainhash.Hash{2})
	next := spendOf(2, head.TxHash())
	c.put(10, head)
	c.put(98, next)
	hints := hintsFor(t, 50, chainbcmr.Hint{Category: chainhash.Hash{1}, Head: head.TxHash(), HeadHeight: 10})
	state := &State{Category: chainhash.Hash{1}}
	b := NewSPVBackend(c, hints)
	resolve(t, b, state)
	if state.Head().Txid != next.TxHash() {
		t.Fatalf("head %+v", state.Head())
	}

	// The link is gone, and comes back in a later block.
	c.replaceFrom(96, 1000)
	replay := spendOf(2, head.TxHash())
	c.put(99, replay)
	c.searches = nil
	if err := b.Advance(context.Background(), []*State{state}, noSave); err != nil {
		t.Fatal(err)
	}
	if state.Head().Height != 99 || len(state.Links) != 2 || state.Status != StatusVerified {
		t.Fatalf("state %+v", state)
	}
	if c.searches[0][0] > 96 {
		t.Fatalf("the scan didn't go back far enough: %v", c.searches)
	}
	if c.searches[0][0] <= 50 {
		t.Fatalf("the scan went below the table: %v", c.searches)
	}
}

// TestSPVReorgBurn ensures a burn a reorganization removed no longer counts.
func TestSPVReorgBurn(t *testing.T) {
	c := newFakeChain(100)
	head := spendOf(1, chainhash.Hash{2})
	c.put(10, head)
	c.put(98, burnOf(head.TxHash()))
	hints := hintsFor(t, 50, chainbcmr.Hint{Category: chainhash.Hash{1}, Head: head.TxHash(), HeadHeight: 10})
	state := &State{Category: chainhash.Hash{1}}
	b := NewSPVBackend(c, hints)
	resolve(t, b, state)
	if !state.Burned {
		t.Fatal("not burned")
	}
	c.replaceFrom(95, 1000)
	if err := b.Advance(context.Background(), []*State{state}, noSave); err != nil {
		t.Fatal(err)
	}
	if state.Burned || state.Head().Txid != head.TxHash() {
		t.Fatalf("state %+v", state)
	}
}

// TestSPVResume ensures a failed scan keeps what it did, and carries on.
func TestSPVResume(t *testing.T) {
	c := newFakeChain(100)
	head := spendOf(1, chainhash.Hash{2})
	c.put(10, head)
	hints := hintsFor(t, 50, chainbcmr.Hint{Category: chainhash.Hash{1}, Head: head.TxHash(), HeadHeight: 10})
	state := &State{Category: chainhash.Hash{1}}
	b := NewSPVBackend(c, hints)
	if err := b.Prepare(context.Background(), []*State{state}); err != nil {
		t.Fatal(err)
	}
	c.failAt = 70
	if err := b.Advance(context.Background(), []*State{state}, noSave); err == nil {
		t.Fatal("no error")
	}
	if state.ScannedTo != 69 || state.Status == StatusVerified {
		t.Fatalf("state %+v", state)
	}
	c.failAt = 0
	c.searches = nil
	if err := b.Advance(context.Background(), []*State{state}, noSave); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.searches, [][2]int32{{70, 100}}) || state.Status != StatusVerified {
		t.Fatalf("searches %v, state %+v", c.searches, state)
	}
}

// TestSPVBlockFailureKeepsPublication ensures a block that can't be read leaves
// the state to try again, rather than start without its publication.
func TestSPVBlockFailureKeepsPublication(t *testing.T) {
	c := newFakeChain(100)
	category := chainhash.Hash{1}
	older := withPublication(t, spendOf(1, chainhash.Hash{2}), 7, "older.example")
	head := spendOf(2, older.TxHash())
	c.put(10, older)
	c.put(20, head)
	hints := hintsFor(t, 50, chainbcmr.Hint{Category: category, Head: head.TxHash(), HeadHeight: 20,
		Publication: &chainbcmr.HintPublication{Txid: older.TxHash(), Height: 10}})
	b := NewSPVBackend(c, hints)
	state := &State{Category: category}

	c.failBlock = true
	if err := b.Prepare(context.Background(), []*State{state}); err != nil {
		t.Fatal(err)
	}
	if len(state.Links) != 0 {
		t.Fatalf("started without the publication: %+v", state)
	}

	c.failBlock = false
	delete(b.waiting, category)
	resolve(t, b, state)
	if pub := state.CurrentPublication(); pub == nil || pub.URIs[0] != "older.example" {
		t.Fatalf("publication %+v", pub)
	}
	if !state.PublicationStale() {
		t.Fatal("a publication below the head isn't marked")
	}
}

// TestSPVBadRowDoesntStopOthers ensures one entry that can't be read leaves the
// other tokens to go on.
func TestSPVBadRowDoesntStopOthers(t *testing.T) {
	c := newFakeChain(100)
	head := spendOf(1, chainhash.Hash{2})
	c.put(10, head)
	bad, good := chainhash.Hash{1}, chainhash.Hash{2}
	hints := hintsFor(t, 50,
		chainbcmr.Hint{Category: bad, Head: chainhash.Hash{0xbb}, HeadHeight: 10},
		chainbcmr.Hint{Category: good, Head: head.TxHash(), HeadHeight: 10})
	badState, goodState := &State{Category: bad}, &State{Category: good}
	resolve(t, NewSPVBackend(c, hints), badState, goodState)
	if goodState.Status != StatusVerified || len(badState.Links) != 0 {
		t.Fatalf("good %+v, bad %+v", goodState, badState)
	}
}

// TestSPVFailedWaits ensures a token that wasn't found isn't searched for on
// every cycle, and that asking again finds it and follows it.
func TestSPVFailedWaits(t *testing.T) {
	c := newFakeChain(100)
	category := chainhash.Hash{1}
	state := &State{Category: category}
	b := NewSPVBackend(c, hintsFor(t, 50))
	resolve(t, b, state)
	if state.Status != StatusFailed {
		t.Fatalf("state %+v", state)
	}
	c.searches = nil
	resolve(t, b, state)
	if len(c.searches) != 0 {
		t.Fatalf("searched again: %v", c.searches)
	}

	genesis := genesisOf(1, category)
	c.put(80, genesis)
	state.Status = StatusSearching // What a refresh does.
	resolve(t, b, state)
	if state.Status != StatusVerified || state.Head().Txid != genesis.TxHash() {
		t.Fatalf("state %+v", state)
	}
}

// TestSPVReorgAboveScanned ensures a link above the scanned height is checked
// too: a genesis that left the chain before the first scan, also a burned one.
func TestSPVReorgAboveScanned(t *testing.T) {
	for _, burned := range []bool{false, true} {
		c := newFakeChain(100)
		category := chainhash.Hash{1}
		genesis := genesisOf(1, category)
		if burned {
			genesis.TxOut = append([]*wire.TxOut{wire.NewTxOut(0, []byte{txscript.OP_RETURN}, wire.TokenData{})},
				genesis.TxOut...)
		}
		c.put(80, genesis)
		state := &State{Category: category}
		b := NewSPVBackend(c, hintsFor(t, 50))
		if err := b.Prepare(context.Background(), []*State{state}); err != nil {
			t.Fatal(err)
		}
		if state.Burned != burned {
			t.Fatalf("burned %v", state.Burned)
		}
		c.replaceFrom(80, 1000)
		if err := b.Advance(context.Background(), []*State{state}, noSave); err != nil {
			t.Fatal(err)
		}
		if len(state.Links) != 0 || state.Status == StatusVerified || state.Burned {
			t.Fatalf("burned %v: state %+v", burned, state)
		}
	}
}

// TestSPVChainErrorKeepsLinks ensures a chain that can't be asked says nothing
// about the links.
func TestSPVChainErrorKeepsLinks(t *testing.T) {
	c := newFakeChain(100)
	head := spendOf(1, chainhash.Hash{2})
	c.put(10, head)
	hints := hintsFor(t, 50, chainbcmr.Hint{Category: chainhash.Hash{1}, Head: head.TxHash(), HeadHeight: 10})
	state := &State{Category: chainhash.Hash{1}}
	b := NewSPVBackend(c, hints)
	resolve(t, b, state)
	b.source = failingHashes{c}
	if err := b.Advance(context.Background(), []*State{state}, noSave); err == nil {
		t.Fatal("no error")
	}
	if len(state.Links) != 1 || state.Status != StatusVerified {
		t.Fatalf("state %+v", state)
	}
}

// failingHashes can't read block hashes.
type failingHashes struct{ *fakeChain }

func (failingHashes) GetBlockHash(int64) (*chainhash.Hash, error) {
	return nil, errors.New("database busy")
}

// TestSPVLinksStayBounded ensures a long chain keeps a bounded number of links,
// and doesn't lose the publication that goes with the old ones.
func TestSPVLinksStayBounded(t *testing.T) {
	c := newFakeChain(100)
	first := withPublication(t, spendOf(1, chainhash.Hash{2}), 7, "first.example")
	c.put(60, first)
	state := &State{Category: chainhash.Hash{1}}
	var txs []*wire.MsgTx
	parent := first.TxHash()
	for i := range maxLinks + 10 {
		tx := spendOf(int64(i+2), parent)
		txs = append(txs, tx)
		parent = tx.TxHash()
	}
	c.put(61, txs...)
	hints := hintsFor(t, 50, chainbcmr.Hint{Category: state.Category, Head: first.TxHash(), HeadHeight: 60})
	resolve(t, NewSPVBackend(c, hints), state)
	if len(state.Links) != maxLinks || state.Head().Txid != parent {
		t.Fatalf("%d links, head %v", len(state.Links), state.Head().Txid)
	}
	if pub := state.CurrentPublication(); pub == nil || pub.URIs[0] != "first.example" {
		t.Fatalf("publication %+v", pub)
	}
}

// TestSPVReorgDuringScan ensures a chain that changes during a scan marks nothing
// as scanned, so that the next pass goes back over what it may have missed.
func TestSPVReorgDuringScan(t *testing.T) {
	c := newFakeChain(100)
	category := chainhash.Hash{1}
	c.put(80, genesisOf(1, category))
	state := &State{Category: category}
	b := NewSPVBackend(c, hintsFor(t, 50))
	if err := b.Prepare(context.Background(), []*State{state}); err != nil {
		t.Fatal(err)
	}
	if err := b.Advance(context.Background(), []*State{state}, noSave); err != nil {
		t.Fatal(err)
	}
	scanned, hash := state.ScannedTo, state.ScannedHash
	for range 3 {
		c.addBlock(1)
	}
	c.duringSearch = func(height int32) {
		c.duringSearch = nil
		c.replaceFrom(height, 2000)
	}
	if err := b.Advance(context.Background(), []*State{state}, noSave); err != nil {
		t.Fatal(err)
	}
	if state.ScannedTo != scanned || state.ScannedHash != hash {
		t.Fatalf("scanned to %d", state.ScannedTo)
	}
}

// TestSPVFailedSearchWaits ensures a genesis search that fails doesn't stop the
// cycle, and leaves the token to try later, or when it's asked for.
func TestSPVFailedSearchWaits(t *testing.T) {
	c := newFakeChain(100)
	c.failAt = 90
	b := NewSPVBackend(c, hintsFor(t, 50))
	searched := &State{Category: chainhash.Hash{2}}
	if err := b.Prepare(context.Background(), []*State{searched}); err != nil {
		t.Fatal(err)
	}
	if searched.Status == StatusFailed {
		t.Fatalf("%+v", searched)
	}
	if _, ok := b.waiting[searched.Category]; !ok {
		t.Fatal("the token doesn't wait")
	}
	b.Forget(searched.Category)
	if _, ok := b.waiting[searched.Category]; ok {
		t.Fatal("the token still waits")
	}
}

// TestSPVReorgDuringGenesisSearch ensures a search that a reorg ran through doesn't
// fail a token whose genesis it may have moved past.
func TestSPVReorgDuringGenesisSearch(t *testing.T) {
	c := newFakeChain(100)
	b := NewSPVBackend(c, hintsFor(t, 50))
	c.duringSearch = func(height int32) {
		c.duringSearch = nil
		c.replaceFrom(95, 3000)
	}
	state := &State{Category: chainhash.Hash{3}}
	if err := b.Prepare(context.Background(), []*State{state}); err != nil {
		t.Fatal(err)
	}
	if state.Status == StatusFailed {
		t.Fatalf("%+v", state)
	}
}

// TestSPVNewBlockDuringGenesisSearch ensures blocks that come during a search are
// searched too, so that the search reaches an answer, and finds a genesis in one.
func TestSPVNewBlockDuringGenesisSearch(t *testing.T) {
	c := newFakeChain(100)
	category := chainhash.Hash{4}
	b := NewSPVBackend(c, hintsFor(t, 50))
	c.duringSearch = func(height int32) {
		c.duringSearch = nil
		c.addBlock(1, genesisOf(1, category))
	}
	state := &State{Category: category}
	if err := b.Prepare(context.Background(), []*State{state}); err != nil {
		t.Fatal(err)
	}
	if len(state.Links) != 1 || state.Status != StatusSearching {
		t.Fatalf("%+v", state)
	}

	c = newFakeChain(100)
	b = NewSPVBackend(c, hintsFor(t, 50))
	c.duringSearch = func(height int32) {
		c.duringSearch = nil
		c.addBlock(1)
	}
	none := &State{Category: chainhash.Hash{5}}
	if err := b.Prepare(context.Background(), []*State{none}); err != nil {
		t.Fatal(err)
	}
	if none.Status != StatusFailed {
		t.Fatalf("%+v", none)
	}
}

// TestSPVFailedFindsGenesisInNewBlock ensures a token that wasn't found, as its
// genesis was unconfirmed, loads when a new block holds it, reading only the new
// blocks.
func TestSPVFailedFindsGenesisInNewBlock(t *testing.T) {
	c := newFakeChain(100)
	category := chainhash.Hash{6}
	state := &State{Category: category}
	b := NewSPVBackend(c, hintsFor(t, 50))
	resolve(t, b, state)
	if state.Status != StatusFailed {
		t.Fatalf("%+v", state)
	}
	c.searches = nil
	c.addBlock(1)
	resolve(t, b, state)
	if state.Status != StatusFailed || !reflect.DeepEqual(c.searches, [][2]int32{{101, 101}}) {
		t.Fatalf("%v, %+v", c.searches, state)
	}
	genesis := genesisOf(1, category)
	c.addBlock(2, genesis)
	resolve(t, b, state)
	if state.Status != StatusVerified || state.Head().Txid != genesis.TxHash() {
		t.Fatalf("%+v", state)
	}
}
