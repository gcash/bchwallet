package bcmr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	chainbcmr "github.com/gcash/bchd/bcmr"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
)

// fakeChain is a chain of blocks, from height 1, that searches them directly.
type fakeChain struct {
	// duringSearch is told of each block a search has read.
	duringSearch func(height int32)

	blocks   []*wire.MsgBlock
	searches [][2]int32

	// outputSearches are the ranges searched for output scripts, and
	// outputReached where the last one stopped.
	outputSearches [][2]int32
	outputReached  int32
	failAt         int32

	failBlock bool

	// hidden are blocks whose spends a search misses, as wrong filters
	// would have it.
	hidden map[int32]bool

	// beforeHash is told of each height whose hash is asked for, before
	// it's given.
	beforeHash func(height int32)

	// reads counts the reads of each block.
	reads map[chainhash.Hash]int
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
	if c.beforeHash != nil {
		c.beforeHash(int32(height))
	}
	if height < 1 || height > int64(len(c.blocks)) {
		return nil, fmt.Errorf("no block at %d", height)
	}
	hash := c.blocks[height-1].BlockHash()
	return &hash, nil
}

func (c *fakeChain) GetBlock(hash *chainhash.Hash, _ <-chan struct{}) (*wire.MsgBlock, error) {
	if c.reads == nil {
		c.reads = make(map[chainhash.Hash]int)
	}
	c.reads[*hash]++
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

func (c *fakeChain) SearchOutputs(scripts [][]byte, from, to int32, found func(OutputFound) bool,
	_ <-chan struct{}) (int32, error) {

	c.outputSearches = append(c.outputSearches, [2]int32{from, to})
	if from < 1 || to < 1 {
		return 0, errors.New("no such block")
	}
	step := int32(1)
	if from > to {
		step = -1
	}
	reached := from - step
	for h := from; h != to+step; h += step {
		if h == c.failAt {
			return reached, errors.New("no peer sent the filter")
		}
		block := c.blocks[h-1]
		for _, tx := range block.Transactions {
			for _, out := range tx.TxOut {
				if !slices.ContainsFunc(scripts, func(s []byte) bool { return bytes.Equal(s, out.PkScript) }) {
					continue
				}
				if !found(OutputFound{Tx: tx, Height: h, BlockHash: block.BlockHash()}) {
					c.outputReached = h
					return h, nil
				}
				break
			}
		}
		reached = h
	}
	c.outputReached = reached
	return reached, nil
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

	if c.hidden[h] {
		return
	}
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

// TestSPVHintBlocksReadOnce ensures tokens whose links from the table share a
// block read it once in a pass.
func TestSPVHintBlocksReadOnce(t *testing.T) {
	c := newFakeChain(100)
	older := withPublication(t, spendOf(1, chainhash.Hash{2}), 7, "older.example")
	first, second := spendOf(2, chainhash.Hash{3}), spendOf(3, chainhash.Hash{4})
	third := spendOf(4, older.TxHash())
	c.put(10, older, first, second)
	c.put(20, third)
	hints := hintsFor(t, 50,
		chainbcmr.Hint{Category: chainhash.Hash{1}, Head: first.TxHash(), HeadHeight: 10},
		chainbcmr.Hint{Category: chainhash.Hash{5}, Head: third.TxHash(), HeadHeight: 20,
			Publication: &chainbcmr.HintPublication{Txid: older.TxHash(), Height: 10}},
		chainbcmr.Hint{Category: chainhash.Hash{6}, Head: second.TxHash(), HeadHeight: 10})
	states := []*State{{Category: chainhash.Hash{5}}, {Category: chainhash.Hash{1}}, {Category: chainhash.Hash{6}}}
	if err := NewSPVBackend(c, hints).Prepare(context.Background(), states); err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if state.Head() == nil {
			t.Fatalf("%+v", state)
		}
	}
	if pub := states[0].CurrentPublication(); pub == nil || pub.URIs[0] != "older.example" {
		t.Fatalf("%+v", states[0])
	}
	for _, height := range []int64{10, 20} {
		hash, err := c.GetBlockHash(height)
		if err != nil {
			t.Fatal(err)
		}
		if c.reads[*hash] != 1 {
			t.Fatalf("block %d read %d times", height, c.reads[*hash])
		}
	}
}

// TestPassBlocksLetGo ensures a pass keeps a block only until its last use.
func TestPassBlocksLetGo(t *testing.T) {
	c := newFakeChain(100)
	first, second := spendOf(1, chainhash.Hash{2}), spendOf(2, chainhash.Hash{3})
	c.put(10, first, second)
	hash, err := c.GetBlockHash(10)
	if err != nil {
		t.Fatal(err)
	}
	blocks := newPassBlocks(c, []*chainbcmr.Hint{{HeadHeight: 10}, {HeadHeight: 10}})
	for i, tx := range []*wire.MsgTx{first, second} {
		if _, err := blocks.readLink(context.Background(), tx.TxHash(), 10, hash); err != nil {
			t.Fatal(err)
		}
		blocks.release(10)
		if kept := len(blocks.blocks); kept != 1-i {
			t.Fatalf("%d blocks kept after %d uses", kept, i+1)
		}
	}
	if c.reads[*hash] != 1 {
		t.Fatalf("the block was read %d times", c.reads[*hash])
	}
}

// TestPassBlocksFailedStartLetsGo ensures a start that fails gives back the
// uses it was counted for, so that the block it didn't get goes with the last
// start that reads it, rather than staying until the pass ends.
func TestPassBlocksFailedStartLetsGo(t *testing.T) {
	c := newFakeChain(100)
	first, second := spendOf(1, chainhash.Hash{2}), spendOf(2, chainhash.Hash{3})
	c.put(10, first, second)
	failing := chainbcmr.Hint{Category: chainhash.Hash{1}, Head: first.TxHash(), HeadHeight: 10}
	starting := chainbcmr.Hint{Category: chainhash.Hash{6}, Head: second.TxHash(), HeadHeight: 10}
	b := NewSPVBackend(c, hintsFor(t, 50, failing, starting))
	blocks := newPassBlocks(c, []*chainbcmr.Hint{&failing, &starting})

	c.failBlock = true
	if err := b.startFromHint(context.Background(), blocks, &State{Category: failing.Category}, &failing); err == nil {
		t.Fatal("a start whose block couldn't be read started")
	}
	c.failBlock = false
	state := &State{Category: starting.Category}
	if err := b.startFromHint(context.Background(), blocks, state, &starting); err != nil {
		t.Fatal(err)
	}
	if state.Head() == nil || len(blocks.blocks) != 0 || len(blocks.uses) != 0 {
		t.Fatalf("after the last start: %d blocks kept, uses %v", len(blocks.blocks), blocks.uses)
	}
}

// TestSPVStartEndsTheWait ensures a token that starts after a start that failed
// no longer waits, from the table and from its genesis alike.
func TestSPVStartEndsTheWait(t *testing.T) {
	c := newFakeChain(100)
	head := spendOf(1, chainhash.Hash{2})
	c.put(10, head)
	hinted := chainhash.Hash{1}
	b := NewSPVBackend(c, hintsFor(t, 50, chainbcmr.Hint{Category: hinted, Head: head.TxHash(), HeadHeight: 10}))
	state := &State{Category: hinted}
	c.failBlock = true
	resolve(t, b, state)
	if _, waits := b.waiting[hinted]; !waits {
		t.Fatal("a start that failed doesn't wait")
	}
	c.failBlock = false
	b.waiting[hinted] = time.Now().Add(-retryInterval)
	resolve(t, b, state)
	if _, waits := b.waiting[hinted]; waits || state.Head() == nil {
		t.Fatalf("still waits after starting: %+v", state)
	}

	genesisCategory := chainhash.Hash{3}
	c.put(80, genesisOf(4, genesisCategory))
	b.waiting[genesisCategory] = time.Now().Add(-retryInterval)
	resolve(t, b, &State{Category: genesisCategory})
	if _, waits := b.waiting[genesisCategory]; waits {
		t.Fatal("still waits after its genesis was found")
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

// TestSPVFailedInNewerTable ensures a token that failed starts from a newer table
// that has it.
func TestSPVFailedInNewerTable(t *testing.T) {
	c := newFakeChain(100)
	category := chainhash.Hash{1}
	genesis := withPublication(t, genesisOf(1, category), 7, "genesis.example")
	c.put(20, genesis)
	state := &State{Category: category}
	resolve(t, NewSPVBackend(c, hintsFor(t, 50)), state)
	if state.Status != StatusFailed {
		t.Fatalf("%+v", state)
	}
	searches := len(c.searches)
	resolve(t, NewSPVBackend(c, hintsFor(t, 50)), state)
	if state.Status != StatusFailed || len(c.searches) != searches {
		t.Fatalf("searched again with the same table: %v, %+v", c.searches, state)
	}
	hints := hintsFor(t, 60, chainbcmr.Hint{Category: category, Head: genesis.TxHash(), HeadHeight: 20})
	resolve(t, NewSPVBackend(c, hints), state)
	if pub := state.CurrentPublication(); state.Status != StatusVerified || pub == nil ||
		pub.URIs[0] != "genesis.example" {

		t.Fatalf("%+v", state)
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

// failedState is a token that wasn't found, up to the tip.
func failedState(t *testing.T, b *SPVBackend, category chainhash.Hash) *State {
	t.Helper()
	state := &State{Category: category}
	resolve(t, b, state)
	if state.Status != StatusFailed || state.ScannedTo == 0 {
		t.Fatalf("%+v", state)
	}
	return state
}

// TestSPVFailedOnBlocksGone ensures a token that wasn't found on blocks a reorg
// took is searched as new, and that one with nothing kept is too.
func TestSPVFailedOnBlocksGone(t *testing.T) {
	c := newFakeChain(100)
	b := NewSPVBackend(c, hintsFor(t, 50))
	gone := failedState(t, b, chainhash.Hash{7})
	gone.ScannedHash = chainhash.Hash{0xee}
	old := failedState(t, b, chainhash.Hash{8})
	old.ScannedTo, old.ScannedHash = 0, chainhash.Hash{}
	stays := failedState(t, b, chainhash.Hash{9})
	if err := b.checkFailed(context.Background(), []*State{gone, old, stays}, noSave); err != nil {
		t.Fatal(err)
	}
	if gone.Status != StatusSearching || old.Status != StatusSearching ||
		stays.Status != StatusFailed {

		t.Fatalf("%+v, %+v, %+v", gone, old, stays)
	}
}

// TestSPVFailedSearchError ensures a search of new blocks that fails moves no token
// on, so that the blocks are searched again.
func TestSPVFailedSearchError(t *testing.T) {
	c := newFakeChain(100)
	b := NewSPVBackend(c, hintsFor(t, 50))
	state := failedState(t, b, chainhash.Hash{10})
	scanned := state.ScannedTo
	c.addBlock(int32(len(c.blocks) + 1))
	c.failAt = 101
	if err := b.checkFailed(context.Background(), []*State{state}, noSave); err == nil {
		t.Fatal("no error")
	}
	if state.ScannedTo != scanned || state.Status != StatusFailed {
		t.Fatalf("%+v", state)
	}
}

// TestSPVFailedDifferentScans ensures tokens searched up to different blocks each
// get the blocks they missed, and all end up at the tip.
func TestSPVFailedDifferentScans(t *testing.T) {
	c := newFakeChain(100)
	b := NewSPVBackend(c, hintsFor(t, 50))
	first := failedState(t, b, chainhash.Hash{11})
	c.addBlock(int32(len(c.blocks) + 1))
	c.addBlock(int32(len(c.blocks) + 1))
	second := failedState(t, b, chainhash.Hash{12})
	c.addBlock(int32(len(c.blocks) + 1))
	c.searches = nil
	if err := b.checkFailed(context.Background(), []*State{first, second}, noSave); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.searches, [][2]int32{{101, 103}}) {
		t.Fatalf("%v", c.searches)
	}
	if first.ScannedTo != 103 || second.ScannedTo != 103 {
		t.Fatalf("%+v, %+v", first, second)
	}
}

// TestSPVNewBlocksKeepComing ensures a token isn't called failed while blocks keep
// coming faster than the search can finish them.
func TestSPVNewBlocksKeepComing(t *testing.T) {
	c := newFakeChain(100)
	b := NewSPVBackend(c, hintsFor(t, 50))
	c.duringSearch = func(int32) { c.addBlock(int32(len(c.blocks) + 1)) }
	state := &State{Category: chainhash.Hash{13}}
	if err := b.Prepare(context.Background(), []*State{state}); err != nil {
		t.Fatal(err)
	}
	if state.Status == StatusFailed {
		t.Fatalf("%+v", state)
	}
}

// TestSPVFailedFoundFollowsOn ensures a token found in a new block is followed from
// there, and not from the tip the search reached, so that a spend in a block after
// its genesis counts.
func TestSPVFailedFoundFollowsOn(t *testing.T) {
	c := newFakeChain(100)
	category := chainhash.Hash{14}
	b := NewSPVBackend(c, hintsFor(t, 50))
	state := failedState(t, b, category)
	genesis := genesisOf(1, category)
	next := spendOf(2, genesis.TxHash())
	c.addBlock(101, genesis)
	c.addBlock(102, next)
	c.addBlock(103)
	resolve(t, b, state)
	if state.Head() == nil || state.Head().Txid != next.TxHash() {
		t.Fatalf("%+v", state)
	}
}

// TestSPVFailedSavesSearch ensures what a search of new blocks did is saved at once,
// as the rest of the cycle can be ended and take it along.
func TestSPVFailedSavesSearch(t *testing.T) {
	c := newFakeChain(100)
	b := NewSPVBackend(c, hintsFor(t, 50))
	state := failedState(t, b, chainhash.Hash{15})
	c.addBlock(101)
	var saved int32
	err := b.checkFailed(context.Background(), []*State{state}, func(s *State) { saved = s.ScannedTo })
	if err != nil {
		t.Fatal(err)
	}
	if saved != 101 {
		t.Fatalf("saved at %d", saved)
	}
}

// authbaseOf returns a transaction with a publication that a token can take its
// category from.
func authbaseOf(t *testing.T, id int64) (*wire.MsgTx, chainhash.Hash) {
	t.Helper()
	tx := withPublication(t, spendOf(id, chainhash.Hash{0xaa}), 0x77, "authbase.example")
	return tx, tx.TxHash()
}

// TestSPVAuthbaseInGenesisBlock ensures a publication on the authbase counts when the
// token has no other, and it's in the block of the genesis.
func TestSPVAuthbaseInGenesisBlock(t *testing.T) {
	c := newFakeChain(100)
	authbase, category := authbaseOf(t, 1)
	c.put(80, authbase, genesisOf(2, category))
	state := &State{Category: category}
	resolve(t, NewSPVBackend(c, hintsFor(t, 50)), state)
	pub := state.CurrentPublication()
	if pub == nil || pub.URIs[0] != "authbase.example" {
		t.Fatalf("%+v", state)
	}
}

// TestSPVLaterPublicationWinsOverAuthbase ensures one on the token's chain comes first.
func TestSPVLaterPublicationWinsOverAuthbase(t *testing.T) {
	c := newFakeChain(100)
	authbase, category := authbaseOf(t, 1)
	genesis := withPublication(t, genesisOf(2, category), 0x78, "genesis.example")
	c.put(80, authbase, genesis)
	state := &State{Category: category}
	resolve(t, NewSPVBackend(c, hintsFor(t, 50)), state)
	if pub := state.CurrentPublication(); pub == nil || pub.URIs[0] != "genesis.example" {
		t.Fatalf("%+v", state)
	}
}

// TestSPVAuthbaseInEarlierBlock ensures a token whose authbase isn't in the block of its
// genesis still loads, with no publication, as nothing tells where that is.
func TestSPVAuthbaseInEarlierBlock(t *testing.T) {
	c := newFakeChain(100)
	authbase, category := authbaseOf(t, 1)
	c.put(70, authbase)
	genesis := genesisOf(2, category)
	c.put(80, genesis)
	state := &State{Category: category}
	resolve(t, NewSPVBackend(c, hintsFor(t, 50)), state)
	if state.Status != StatusVerified || state.Head().Txid != genesis.TxHash() {
		t.Fatalf("%+v", state)
	}
	if state.CurrentPublication() != nil {
		t.Fatalf("%+v", state)
	}
}

// TestSPVAuthbaseBlockUnread ensures a block that can't be read leaves the token to be
// found again, as this is the only look at its authbase.
func TestSPVAuthbaseBlockUnread(t *testing.T) {
	c := newFakeChain(100)
	authbase, category := authbaseOf(t, 1)
	c.put(80, authbase, genesisOf(2, category))
	c.failBlock = true
	state := &State{Category: category}
	b := NewSPVBackend(c, hintsFor(t, 50))
	if err := b.Prepare(context.Background(), []*State{state}); err != nil {
		t.Fatal(err)
	}
	if len(state.Links) != 0 || state.Status == StatusFailed {
		t.Fatalf("%+v", state)
	}

	c.failBlock = false
	delete(b.waiting, category)
	if err := b.Prepare(context.Background(), []*State{state}); err != nil {
		t.Fatal(err)
	}
	if pub := state.CurrentPublication(); pub == nil || pub.URIs[0] != "authbase.example" {
		t.Fatalf("%+v", state)
	}
}

// authbaseKey is the public key an authbase in these tests pays to.
var authbaseKey = append([]byte{0x02}, bytes.Repeat([]byte{7}, 32)...)

// p2pkhAuthbase returns an authbase whose output 0 pays to authbaseKey, with a
// publication if publishes, and the token's genesis spending it as that key does.
func p2pkhAuthbase(t *testing.T, id int64, publishes bool) (*wire.MsgTx, *wire.MsgTx, chainhash.Hash) {
	t.Helper()
	authbase := wire.NewMsgTx(2)
	authbase.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{0xab, byte(id)}}, nil))
	authbase.AddTxOut(wire.NewTxOut(id, p2pkh(bchutil.Hash160(authbaseKey)), wire.TokenData{}))
	if publishes {
		authbase = withPublication(t, authbase, 0x79, "authbase.example")
	}
	category := authbase.TxHash()
	genesis := genesisOf(id+1, category)
	sigScript, err := txscript.NewScriptBuilder().AddData(bytes.Repeat([]byte{9}, 65)).AddData(authbaseKey).Script()
	if err != nil {
		t.Fatal(err)
	}
	genesis.TxIn[0].SignatureScript = sigScript
	return authbase, genesis, category
}

// TestSPVAuthbaseSearch ensures a token with no publication on its chain has its
// authbase looked for in the week before its genesis, once, and that one found
// there gives the token its publication.
func TestSPVAuthbaseSearch(t *testing.T) {
	c := newFakeChain(100)
	authbase, genesis, category := p2pkhAuthbase(t, 1, true)
	c.put(75, authbase)
	c.put(80, genesis)
	b := NewSPVBackend(c, hintsFor(t, 50))
	state := &State{Category: category}
	resolve(t, b, state)
	if pub := state.CurrentPublication(); pub == nil || pub.URIs[0] != "authbase.example" {
		t.Fatalf("%+v", state)
	}
	if !state.AuthbaseSearched || !reflect.DeepEqual(c.outputSearches, [][2]int32{{79, 1}}) || c.outputReached != 75 {
		t.Fatalf("searched %v to %v, %+v", c.outputSearches, c.outputReached, state)
	}
	c.outputSearches = nil
	resolve(t, b, state)
	if len(c.outputSearches) != 0 {
		t.Fatalf("searched again: %v", c.outputSearches)
	}
}

// TestSPVAuthbaseSearchReach ensures the search goes back a week and no further, and
// that a token whose authbase is further back has no publication, for good.
func TestSPVAuthbaseSearchReach(t *testing.T) {
	c := newFakeChain(1200)
	authbase, genesis, category := p2pkhAuthbase(t, 1, true)
	c.put(100, authbase)
	c.put(1150, genesis)
	b := NewSPVBackend(c, hintsFor(t, 50))
	state := &State{Category: category}
	resolve(t, b, state)
	if state.CurrentPublication() != nil || !state.AuthbaseSearched {
		t.Fatalf("%+v", state)
	}
	if !reflect.DeepEqual(c.outputSearches, [][2]int32{{1149, 1150 - authbaseReach}}) {
		t.Fatalf("searched %v", c.outputSearches)
	}
	c.outputSearches = nil
	resolve(t, b, state)
	if len(c.outputSearches) != 0 {
		t.Fatalf("searched again: %v", c.outputSearches)
	}
}

// TestSPVAuthbaseSearchNotNeeded ensures no search for a token with a publication on
// its chain, from the table, with an authbase in its genesis block, or whose
// authbase's script can't be rebuilt.
func TestSPVAuthbaseSearchNotNeeded(t *testing.T) {
	// A publication on the genesis.
	c := newFakeChain(100)
	authbase, genesis, category := p2pkhAuthbase(t, 1, true)
	genesis = withPublication(t, genesis, 0x7a, "genesis.example")
	c.put(75, authbase)
	c.put(80, genesis)
	state := &State{Category: category}
	resolve(t, NewSPVBackend(c, hintsFor(t, 50)), state)
	if len(c.outputSearches) != 0 {
		t.Fatalf("searched with a publication: %v", c.outputSearches)
	}

	// In the genesis block, which the free check reads.
	c = newFakeChain(100)
	authbase, genesis, category = p2pkhAuthbase(t, 2, true)
	c.put(80, authbase, genesis)
	state = &State{Category: category}
	resolve(t, NewSPVBackend(c, hintsFor(t, 50)), state)
	if len(c.outputSearches) != 0 || state.CurrentPublication() == nil {
		t.Fatalf("searched %v, %+v", c.outputSearches, state)
	}

	// In the genesis block, with no publication.
	c = newFakeChain(100)
	authbase, genesis, category = p2pkhAuthbase(t, 4, false)
	c.put(80, authbase, genesis)
	state = &State{Category: category}
	resolve(t, NewSPVBackend(c, hintsFor(t, 50)), state)
	if len(c.outputSearches) != 0 || !state.AuthbaseSearched {
		t.Fatalf("searched %v, %+v", c.outputSearches, state)
	}

	// A script that can't be rebuilt.
	c = newFakeChain(100)
	authbase, genesis, category = p2pkhAuthbase(t, 3, true)
	genesis.TxIn[0].SignatureScript = []byte{txscript.OP_DUP}
	c.put(75, authbase)
	c.put(80, genesis)
	state = &State{Category: category}
	resolve(t, NewSPVBackend(c, hintsFor(t, 50)), state)
	if len(c.outputSearches) != 0 || !state.AuthbaseSearched {
		t.Fatalf("searched %v, %+v", c.outputSearches, state)
	}

	// From the table, which has the publication.
	c = newFakeChain(100)
	head := spendOf(9, chainhash.Hash{0x99})
	c.put(10, head)
	hints := hintsFor(t, 50, chainbcmr.Hint{Category: chainhash.Hash{0x98}, Head: head.TxHash(), HeadHeight: 10})
	state = &State{Category: chainhash.Hash{0x98}}
	resolve(t, NewSPVBackend(c, hints), state)
	if len(c.outputSearches) != 0 {
		t.Fatalf("searched for a token from the table: %v", c.outputSearches)
	}
}

// TestSPVAuthbaseSearchFails ensures a search that fails is tried again after a
// wait that doubles with each failure, or at once when the token is asked for
// again, and that a reorg that takes the genesis away forgets the authbase.
func TestSPVAuthbaseSearchFails(t *testing.T) {
	c := newFakeChain(100)
	authbase, genesis, category := p2pkhAuthbase(t, 1, true)
	c.put(75, authbase)
	c.put(80, genesis)
	b := NewSPVBackend(c, hintsFor(t, 50))
	state := &State{Category: category}
	c.failAt = 77
	resolve(t, b, state)
	if state.AuthbaseSearched || state.CurrentPublication() != nil {
		t.Fatalf("%+v", state)
	}
	searches := func() int {
		t.Helper()
		before := len(c.outputSearches)
		resolve(t, b, state)
		return len(c.outputSearches) - before
	}
	back := func(by time.Duration) {
		tried := b.searchTried[category]
		tried.last = tried.last.Add(-by)
		b.searchTried[category] = tried
	}
	if n := searches(); n != 0 {
		t.Fatalf("%d searches right after a failure", n)
	}
	back(retryInterval)
	if n := searches(); n != 1 || state.AuthbaseSearched {
		t.Fatalf("%d searches after the wait, %+v", n, state)
	}
	back(retryInterval)
	if n := searches(); n != 0 {
		t.Fatalf("%d searches before the second wait", n)
	}
	b.Forget(category)
	c.failAt = 0
	if n := searches(); n != 1 || !state.AuthbaseSearched || state.CurrentPublication() == nil {
		t.Fatalf("%d searches when asked for again, %+v", n, state)
	}

	// A new genesis has a search of its own, at once.
	b.searchTried[category] = retry{last: time.Now(), tries: 3}
	c.replaceFrom(60, 1000)
	if err := b.rewind(state); err != nil {
		t.Fatal(err)
	}
	if state.AuthbaseSearched || state.AuthbaseScripts != nil || state.GenesisHeight != 0 || state.Older != nil {
		t.Fatalf("%+v", state)
	}
	if _, ok := b.searchTried[category]; ok {
		t.Fatal("the failed search still waits")
	}
}

// TestSPVAuthbaseSearchShared ensures tokens whose weeks overlap share one search,
// that one far from them has its own, and that an authbase outside its token's
// week doesn't count.
func TestSPVAuthbaseSearchShared(t *testing.T) {
	c := newFakeChain(3000)
	authbaseA, genesisA, a := p2pkhAuthbase(t, 1, true)
	authbaseB, genesisB, b := p2pkhAuthbase(t, 3, true)
	authbaseC, genesisC, cat := p2pkhAuthbase(t, 5, true)
	authbaseD, genesisD, d := p2pkhAuthbase(t, 7, true)
	c.put(1700, authbaseA)
	c.put(1800, genesisA)
	c.put(1500, authbaseB)
	c.put(1600, genesisB)
	c.put(2850, authbaseC)
	c.put(2900, genesisC)
	c.put(700, authbaseD) // Below D's week, in B's.
	c.put(1750, genesisD)
	states := []*State{{Category: a}, {Category: b}, {Category: cat}, {Category: d}}
	resolve(t, NewSPVBackend(c, hintsFor(t, 50)), states...)
	if want := [][2]int32{{2899, 2900 - authbaseReach}, {1799, 1600 - authbaseReach}}; !reflect.DeepEqual(c.outputSearches, want) {
		t.Fatalf("searched %v, not %v", c.outputSearches, want)
	}
	for _, state := range states[:3] {
		if !state.AuthbaseSearched || state.CurrentPublication() == nil {
			t.Fatalf("%+v", state)
		}
	}
	if !states[3].AuthbaseSearched || states[3].CurrentPublication() != nil {
		t.Fatalf("found outside its week: %+v", states[3])
	}
}

// TestSPVAuthbaseSearchWeeksMeet ensures tokens whose weeks meet, with no block
// between them, share a search.
func TestSPVAuthbaseSearchWeeksMeet(t *testing.T) {
	c := newFakeChain(3000)
	_, genesisP, p := p2pkhAuthbase(t, 1, true)
	_, genesisQ, q := p2pkhAuthbase(t, 3, true)
	c.put(2500, genesisP)
	c.put(2500-authbaseReach, genesisQ)
	resolve(t, NewSPVBackend(c, hintsFor(t, 50)), &State{Category: p}, &State{Category: q})
	if want := [][2]int32{{2499, 2500 - 2*authbaseReach}}; !reflect.DeepEqual(c.outputSearches, want) {
		t.Fatalf("searched %v, not %v", c.outputSearches, want)
	}
}

// TestSPVAuthbaseSearchSharedFails ensures a shared search that fails counts for
// the tokens whose weeks it went through, and has the others wait.
func TestSPVAuthbaseSearchSharedFails(t *testing.T) {
	c := newFakeChain(3000)
	_, genesisE, e := p2pkhAuthbase(t, 1, true)
	authbaseF, genesisF, f := p2pkhAuthbase(t, 3, true)
	c.put(2700, genesisE)
	c.put(900, authbaseF)
	c.put(1800, genesisF)
	c.failAt = 2700 - authbaseReach - 1 // The search fails just past E's week.
	backend := NewSPVBackend(c, hintsFor(t, 50))
	states := []*State{{Category: e}, {Category: f}}
	resolve(t, backend, states...)
	if len(c.outputSearches) != 1 || !states[0].AuthbaseSearched || states[1].AuthbaseSearched {
		t.Fatalf("searched %v, %+v, %+v", c.outputSearches, states[0], states[1])
	}
	if _, waits := backend.searchTried[e]; waits || backend.searchTried[f].tries != 1 {
		t.Fatalf("%v", backend.searchTried)
	}
}

// TestSPVAuthbaseSearchOtherPayments ensures payments to the authbase's script
// that aren't the authbase are passed over, publication or not.
func TestSPVAuthbaseSearchOtherPayments(t *testing.T) {
	c := newFakeChain(100)
	authbase, genesis, category := p2pkhAuthbase(t, 1, true)
	other := wire.NewMsgTx(2)
	other.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{0xcd}}, nil))
	other.AddTxOut(wire.NewTxOut(5, p2pkh(bchutil.Hash160(authbaseKey)), wire.TokenData{}))
	other = withPublication(t, other, 0x7b, "other.example")
	c.put(75, authbase)
	c.put(78, other)
	c.put(80, genesis)
	state := &State{Category: category}
	resolve(t, NewSPVBackend(c, hintsFor(t, 50)), state)
	if pub := state.CurrentPublication(); pub == nil || pub.URIs[0] != "authbase.example" {
		t.Fatalf("%+v", state)
	}
}

// TestSPVAuthbaseSearchWaitsForTheWalk ensures a token whose walk hasn't reached
// the tip isn't searched, as a publication may still be ahead.
func TestSPVAuthbaseSearchWaitsForTheWalk(t *testing.T) {
	c := newFakeChain(100)
	authbase, genesis, category := p2pkhAuthbase(t, 1, true)
	c.put(75, authbase)
	c.put(80, genesis)
	b := NewSPVBackend(c, hintsFor(t, 50))
	state := &State{Category: category}
	if err := b.Prepare(context.Background(), []*State{state}); err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusSearching || len(state.AuthbaseScripts) == 0 {
		t.Fatalf("%+v", state)
	}
	if err := b.searchAuthbases(context.Background(), []*State{state}, noSave); err != nil {
		t.Fatal(err)
	}
	if len(c.outputSearches) != 0 || state.AuthbaseSearched {
		t.Fatalf("searched %v, %+v", c.outputSearches, state)
	}
}

// TestSPVAuthbaseSearchBurned ensures a burned token has its authbase looked for,
// as what it published still names it, even on a pass with nothing to watch.
func TestSPVAuthbaseSearchBurned(t *testing.T) {
	c := newFakeChain(100)
	authbase, genesis, category := p2pkhAuthbase(t, 1, true)
	c.put(75, authbase)
	c.put(80, genesis)
	c.put(85, burnOf(genesis.TxHash()))
	b := NewSPVBackend(c, hintsFor(t, 50))
	state := &State{Category: category}
	c.failAt = 77
	resolve(t, b, state)
	if !state.Burned || state.AuthbaseSearched {
		t.Fatalf("%+v", state)
	}
	c.failAt = 0
	b.Forget(category) // As asking for it again does, rather than waiting.
	if err := b.Advance(context.Background(), []*State{state}, noSave); err != nil {
		t.Fatal(err)
	}
	if pub := state.CurrentPublication(); pub == nil || pub.URIs[0] != "authbase.example" {
		t.Fatalf("%+v", state)
	}
}

// TestSPVRolledBack ensures a rollback of the filter headers sets back what a
// state's scans covered above its height, but not below the table's, and has an
// authbase search that reached above it done again.
func TestSPVRolledBack(t *testing.T) {
	c := newFakeChain(100)
	b := NewSPVBackend(c, hintsFor(t, 50))
	state := &State{ScannedTo: 100, AuthbaseScripts: [][]byte{{1}}, GenesisHeight: 90,
		AuthbaseSearched: true}
	changed, err := b.RolledBack([]*State{state}, 80)
	if err != nil || len(changed) != 1 {
		t.Fatalf("changed %v, %v", changed, err)
	}
	if state.ScannedTo != 80 || state.ScannedHash != c.blocks[79].BlockHash() || state.AuthbaseSearched {
		t.Fatalf("%+v", state)
	}
	if changed, _ := b.RolledBack([]*State{state}, 85); len(changed) != 0 {
		t.Fatal("a rollback above the scan changed it")
	}

	// The table vouches for what's below its height, and a search that
	// ended below the rollback stands.
	state = &State{ScannedTo: 100, AuthbaseScripts: [][]byte{{1}}, GenesisHeight: 30,
		AuthbaseSearched: true}
	if changed, err := b.RolledBack([]*State{state}, 20); err != nil || len(changed) != 1 {
		t.Fatalf("changed %v, %v", changed, err)
	}
	if state.ScannedTo != 50 || !state.AuthbaseSearched {
		t.Fatalf("%+v", state)
	}
}

// hashCounting is a chain that counts the hashes asked for, and fails them if
// fail is set.
type hashCounting struct {
	*fakeChain
	hashes int
	fail   bool
}

func (c *hashCounting) GetBlockHash(height int64) (*chainhash.Hash, error) {
	c.hashes++
	if c.fail {
		return nil, errors.New("no hash")
	}
	return c.fakeChain.GetBlockHash(height)
}

// TestSPVRolledBackWithoutHash ensures a rollback whose block hash can't be read
// leaves the scans as they were, but still sets back each search that reached
// above it, the later states' too, and says it failed.
func TestSPVRolledBackWithoutHash(t *testing.T) {
	c := &hashCounting{fakeChain: newFakeChain(100), fail: true}
	b := NewSPVBackend(c, hintsFor(t, 50))
	scanned := &State{ScannedTo: 100}
	searched := &State{ScannedTo: 100, AuthbaseScripts: [][]byte{{1}}, GenesisHeight: 90, AuthbaseSearched: true}
	changed, err := b.RolledBack([]*State{scanned, searched}, 80)
	if err == nil {
		t.Fatal("no error")
	}
	if scanned.ScannedTo != 100 || searched.ScannedTo != 100 || searched.AuthbaseSearched {
		t.Fatalf("scanned %d, %d, searched %v", scanned.ScannedTo, searched.ScannedTo, searched.AuthbaseSearched)
	}
	if len(changed) != 1 || changed[0] != searched {
		t.Fatalf("%d states changed", len(changed))
	}
}

// TestSPVRolledBackTogether ensures a rollback of many states looks up the hash
// of the block they're set back to once, and returns only those it changed.
func TestSPVRolledBackTogether(t *testing.T) {
	c := &hashCounting{fakeChain: newFakeChain(100)}
	b := NewSPVBackend(c, hintsFor(t, 50))
	var states []*State
	for range 5 {
		states = append(states, &State{ScannedTo: 100})
	}
	below := &State{ScannedTo: 70}
	states = append(states, below)
	changed, err := b.RolledBack(states, 80)
	if err != nil {
		t.Fatal(err)
	}
	if c.hashes != 1 {
		t.Fatalf("%d hashes looked up", c.hashes)
	}
	if len(changed) != 5 || slices.Contains(changed, below) {
		t.Fatalf("%d states changed", len(changed))
	}
}

// TestSPVRolledBackFindsHiddenSpend ensures a spend that wrong filters hid is
// found once the filter headers are rolled back below it.
func TestSPVRolledBackFindsHiddenSpend(t *testing.T) {
	c := newFakeChain(100)
	category := chainhash.Hash{1}
	head := spendOf(1, chainhash.Hash{2})
	next := withPublication(t, spendOf(2, head.TxHash()), 8, "next.example")
	c.put(10, head)
	c.put(70, next)
	c.hidden = map[int32]bool{70: true}
	hints := hintsFor(t, 50, chainbcmr.Hint{Category: category, Head: head.TxHash(), HeadHeight: 10})
	b := NewSPVBackend(c, hints)
	state := &State{Category: category}
	resolve(t, b, state)
	if state.Head().Txid != head.TxHash() || state.ScannedTo != 100 {
		t.Fatalf("head %v, scanned to %d", state.Head().Txid, state.ScannedTo)
	}

	c.hidden = nil
	if _, err := b.RolledBack([]*State{state}, 60); err != nil {
		t.Fatal(err)
	}
	resolve(t, b, state)
	if state.Head().Txid != next.TxHash() {
		t.Fatalf("the hidden spend wasn't found: head %v", state.Head().Txid)
	}
}

// TestSPVMarkScannedReorg ensures a reorganization just as the hash to keep is
// read leaves the state as it was, rather than keep a hash of a block that wasn't
// scanned.
func TestSPVMarkScannedReorg(t *testing.T) {
	c := newFakeChain(100)
	b := NewSPVBackend(c, nil)
	state := &State{ScannedTo: 10, ScannedHash: c.blocks[9].BlockHash()}
	tipHash := c.blocks[99].BlockHash()
	reorged := false
	c.beforeHash = func(height int32) {
		if height == 90 && !reorged {
			reorged = true
			c.replaceFrom(50, 1000)
		}
	}
	if err := b.markScanned([]*State{state}, 90, 100, &tipHash, noSave); err != nil {
		t.Fatal(err)
	}
	if state.ScannedTo != 10 {
		t.Fatalf("scanned to %d of a chain that changed", state.ScannedTo)
	}
}
