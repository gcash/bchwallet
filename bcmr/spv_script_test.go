package bcmr

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
)

// scriptFilterChains marks the fake chains whose block filters are script filters.
var scriptFilterChains = map[*fakeChain]bool{}

// scriptSpendCalls and categoryOutputCalls count how often a fake chain was
// asked for the script-filter searches, and scriptSpendLooked and categoryLooked
// the heights those searches read, which are only the blocks a script filter
// would match.
var (
	scriptSpendCalls    = map[*fakeChain]int{}
	categoryOutputCalls = map[*fakeChain]int{}
	scriptSpendLooked   = map[*fakeChain][]int32{}
	categoryLooked      = map[*fakeChain][]int32{}
)

func (c *fakeChain) ScriptFilters() bool {
	return scriptFilterChains[c]
}

// SearchScriptSpends walks the chain as SearchSpends does, but reads a block
// only when it creates or spends an output with one of the watched scripts.
func (c *fakeChain) SearchScriptSpends(inputs []ScriptOutPoint, from, to int32,
	found func(SpendFound) []ScriptOutPoint, progress func(height int32),
	_ <-chan struct{}) (int32, error) {

	scriptSpendCalls[c]++
	step := int32(1)
	if from > to {
		step = -1
	}
	watched := make(map[wire.OutPoint]bool, len(inputs))
	scripts := make(map[wire.OutPoint][]byte, len(inputs))
	for _, input := range inputs {
		watched[input.OutPoint] = true
		scripts[input.OutPoint] = input.PkScript
	}
	reached := from - step
	for h := from; h != to+step && len(watched) > 0; h += step {
		if h == c.failAt {
			return reached, errors.New("no peer sent the filter")
		}
		if c.blockTouchesScripts(h, watchedScripts(watched, scripts)) {
			scriptSpendLooked[c] = append(scriptSpendLooked[c], h)
			c.searchBlock(h, watched, func(spend SpendFound) []wire.OutPoint {
				if found == nil {
					return nil
				}
				next := found(spend)
				outpoints := make([]wire.OutPoint, len(next))
				for i, input := range next {
					outpoints[i] = input.OutPoint
					scripts[input.OutPoint] = input.PkScript
				}
				return outpoints
			}, step > 0)
			if c.duringSearch != nil {
				c.duringSearch(h)
			}
		}
		reached = h
		if progress != nil {
			progress(h)
		}
	}
	return reached, nil
}

// SearchCategoryOutputs walks the chain from `from` to `to` and reports each
// transaction that creates an output of one of the categories. A block is read
// only when it creates or spends such an output, and a block that only spends
// one reports nothing.
func (c *fakeChain) SearchCategoryOutputs(categories []chainhash.Hash, from, to int32,
	found func(OutputFound) bool, _ <-chan struct{}) (int32, error) {

	categoryOutputCalls[c]++
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
		if c.blockTouchesCategories(h, categories) {
			categoryLooked[c] = append(categoryLooked[c], h)
			block := c.blocks[h-1]
			stop := false
			for _, tx := range block.Transactions {
				if !createsCategory(tx, categories) {
					continue
				}
				if found != nil && !found(OutputFound{Tx: tx, Height: h, BlockHash: block.BlockHash()}) {
					stop = true
					break
				}
			}
			if stop {
				return h, nil
			}
		}
		reached = h
	}
	return reached, nil
}

// blockTouchesScripts reports whether a block creates or spends an output whose
// locking script is one of scripts, which is when a script filter matches it.
func (c *fakeChain) blockTouchesScripts(h int32, scripts [][]byte) bool {
	block := c.blocks[h-1]
	for _, tx := range block.Transactions {
		for _, out := range tx.TxOut {
			if scriptWatched(out.PkScript, scripts) {
				return true
			}
		}
		for _, in := range tx.TxIn {
			if scriptWatched(c.lockingScript(in.PreviousOutPoint), scripts) {
				return true
			}
		}
	}
	return false
}

// blockTouchesCategories reports whether a block creates or spends an output
// with tokens of one of the categories, which is when a script filter matches it.
func (c *fakeChain) blockTouchesCategories(h int32, categories []chainhash.Hash) bool {
	block := c.blocks[h-1]
	for _, tx := range block.Transactions {
		if createsCategory(tx, categories) {
			return true
		}
		for _, in := range tx.TxIn {
			out := c.txOut(in.PreviousOutPoint)
			if out != nil && carriesWatched(out, categories) {
				return true
			}
		}
	}
	return false
}

// lockingScript is the locking script of an output the chain created, or nil
// when that output isn't in it.
func (c *fakeChain) lockingScript(op wire.OutPoint) []byte {
	if out := c.txOut(op); out != nil {
		return out.PkScript
	}
	return nil
}

func (c *fakeChain) txOut(op wire.OutPoint) *wire.TxOut {
	for _, block := range c.blocks {
		for _, tx := range block.Transactions {
			if tx.TxHash() != op.Hash {
				continue
			}
			if op.Index >= uint32(len(tx.TxOut)) {
				return nil
			}
			return tx.TxOut[op.Index]
		}
	}
	return nil
}

func watchedScripts(watched map[wire.OutPoint]bool, scripts map[wire.OutPoint][]byte) [][]byte {
	var out [][]byte
	seen := make(map[string]struct{})
	for outpoint, on := range watched {
		if !on {
			continue
		}
		script := scripts[outpoint]
		if _, ok := seen[string(script)]; ok {
			continue
		}
		seen[string(script)] = struct{}{}
		out = append(out, script)
	}
	return out
}

func scriptWatched(script []byte, watched [][]byte) bool {
	for _, want := range watched {
		if bytes.Equal(script, want) {
			return true
		}
	}
	return false
}

func createsCategory(tx *wire.MsgTx, categories []chainhash.Hash) bool {
	for _, out := range tx.TxOut {
		if carriesWatched(out, categories) {
			return true
		}
	}
	return false
}

func carriesWatched(out *wire.TxOut, categories []chainhash.Hash) bool {
	if out.TokenData.IsEmpty() {
		return false
	}
	return slices.Contains(categories, chainhash.Hash(out.TokenData.CategoryID))
}

func withScriptFilters(c *fakeChain) *fakeChain {
	scriptFilterChains[c] = true
	return c
}

// followedToken puts one token on c: its genesis, two further links in the next
// block, and one link above that. Output 0 of each link pays a script of its
// own, so a script filter matches those blocks and no others.
func followedToken(t *testing.T, c *fakeChain) (chainhash.Hash, []*wire.MsgTx) {
	t.Helper()
	category := chainhash.Hash{1}
	genesis := withPublication(t, genesisOf(1, category), 7, "genesis.example")
	genesis.TxOut[0].PkScript = []byte{txscript.OP_NOP, 0x11}
	hop := spendOf(2, genesis.TxHash())
	hop.TxOut[0].PkScript = []byte{txscript.OP_NOP, 0x12}
	last := withPublication(t, spendOf(3, hop.TxHash()), 8, "last.example")
	last.TxOut[0].PkScript = []byte{txscript.OP_NOP, 0x13}
	tail := spendOf(4, last.TxHash())
	tail.TxOut[0].PkScript = []byte{txscript.OP_NOP, 0x14}
	c.put(60, genesis)
	c.put(70, hop, last)
	c.put(80, tail)
	return category, []*wire.MsgTx{genesis, hop, last, tail}
}

func resolveToken(t *testing.T, c *fakeChain, category chainhash.Hash) *State {
	t.Helper()
	state := &State{Category: category}
	resolve(t, NewSPVBackend(c, hintsFor(t, 50)), state)
	return state
}

// TestSPVScriptFiltersFollowAuthchain ensures that on script filters a token's
// genesis is found by its category and its authchain is followed to the head,
// and that the state is the one the same chain gives on regular filters.
func TestSPVScriptFiltersFollowAuthchain(t *testing.T) {
	regularChain := newFakeChain(100)
	regularCategory, regularTxs := followedToken(t, regularChain)
	regular := resolveToken(t, regularChain, regularCategory)

	scriptChain := withScriptFilters(newFakeChain(100))
	scriptCategory, scriptTxs := followedToken(t, scriptChain)
	script := resolveToken(t, scriptChain, scriptCategory)

	if !reflect.DeepEqual(regular, script) {
		t.Fatalf("regular %+v\nscript %+v", regular, script)
	}
	if script.Head() == nil || script.Head().Txid != scriptTxs[len(scriptTxs)-1].TxHash() || len(script.Links) != len(scriptTxs) {
		t.Fatalf("head %+v, %d links", script.Head(), len(script.Links))
	}
	if pub := script.CurrentPublication(); pub == nil || pub.URIs[0] != "last.example" {
		t.Fatalf("publication %+v", pub)
	}
	if regularTxs[0].TxHash() != scriptTxs[0].TxHash() {
		t.Fatalf("chains differ at the genesis")
	}
	if scriptSpendCalls[regularChain] != 0 || categoryOutputCalls[regularChain] != 0 {
		t.Fatalf("regular filters used script searches: spends %d, categories %d",
			scriptSpendCalls[regularChain], categoryOutputCalls[regularChain])
	}
	if !slices.Equal(scriptSpendLooked[scriptChain], []int32{60, 70, 80}) {
		t.Fatalf("blocks read for spends %v", scriptSpendLooked[scriptChain])
	}
}

// TestSPVScriptFiltersTwoGenesesInOneBlock ensures two tokens created in the
// same block are both found on script filters.
func TestSPVScriptFiltersTwoGenesesInOneBlock(t *testing.T) {
	c := withScriptFilters(newFakeChain(100))
	catA, catB := chainhash.Hash{1}, chainhash.Hash{2}
	genA, genB := genesisOf(1, catA), genesisOf(2, catB)
	c.put(80, genA, genB)
	stateA, stateB := &State{Category: catA}, &State{Category: catB}
	resolve(t, NewSPVBackend(c, hintsFor(t, 50)), stateA, stateB)
	if stateA.Head() == nil || stateA.Head().Txid != genA.TxHash() || stateA.Status != StatusVerified {
		t.Fatalf("first %+v", stateA)
	}
	if stateB.Head() == nil || stateB.Head().Txid != genB.TxHash() || stateB.Status != StatusVerified {
		t.Fatalf("second %+v", stateB)
	}
}

// TestSPVScriptFiltersTransferIsNotGenesis ensures a search above a token's
// genesis that sees a transfer of the category doesn't take the transfer for
// the genesis or change the token's state.
func TestSPVScriptFiltersTransferIsNotGenesis(t *testing.T) {
	c := withScriptFilters(newFakeChain(100))
	category := chainhash.Hash{1}
	genesis := genesisOf(1, category)
	c.put(60, genesis)
	transfer := spendOf(9, chainhash.Hash{0x99})
	var id [32]byte
	copy(id[:], category[:])
	transfer.AddTxOut(wire.NewTxOut(1, []byte{txscript.OP_TRUE}, wire.TokenData{
		CategoryID: id, Amount: 5, BitField: wire.HAS_AMOUNT,
	}))
	c.put(90, transfer)

	b := NewSPVBackend(c, hintsFor(t, 50))
	state := &State{Category: category}
	resolve(t, b, state)
	if state.Head() == nil || state.Head().Txid != genesis.TxHash() || len(state.Links) != 1 {
		t.Fatalf("after the genesis search %+v", state)
	}
	before := append([]Link(nil), state.Links...)
	reported := false
	if _, err := b.searchGenesisSpends(context.Background(), []wire.OutPoint{{Hash: category}}, 70, 100,
		func(SpendFound) []wire.OutPoint {
			reported = true
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	if reported {
		t.Fatal("a transfer above the genesis was taken for the genesis")
	}
	if !reflect.DeepEqual(state.Links, before) || state.Status != StatusVerified {
		t.Fatalf("state changed %+v", state)
	}
}

// TestSPVScriptFiltersBurnEndsAuthchain ensures an authchain whose output 0 is
// an OP_RETURN ends there on script filters, as it does on regular filters.
func TestSPVScriptFiltersBurnEndsAuthchain(t *testing.T) {
	burned := func(t *testing.T, script bool) *State {
		t.Helper()
		c := newFakeChain(100)
		if script {
			withScriptFilters(c)
		}
		category := chainhash.Hash{1}
		genesis := genesisOf(1, category)
		burn := burnOf(genesis.TxHash())
		c.put(60, genesis)
		c.put(70, burn)
		state := resolveToken(t, c, category)
		if !state.Burned || state.Head() == nil || state.Head().Txid != burn.TxHash() {
			t.Fatalf("script %v: %+v", script, state)
		}
		return state
	}
	regular, script := burned(t, false), burned(t, true)
	if !reflect.DeepEqual(regular, script) {
		t.Fatalf("regular %+v\nscript %+v", regular, script)
	}
}

// TestSPVScriptFiltersReadLinkScriptOnce ensures a fresh backend reads the block
// of a stored link once to learn the script of its output 0, then follows the
// link.
func TestSPVScriptFiltersReadLinkScriptOnce(t *testing.T) {
	c := withScriptFilters(newFakeChain(100))
	head := spendOf(1, chainhash.Hash{2})
	next := spendOf(2, head.TxHash())
	c.put(10, head)
	c.put(60, next)
	hash, err := c.GetBlockHash(10)
	if err != nil {
		t.Fatal(err)
	}
	state := &State{
		Category:    chainhash.Hash{1},
		Links:       []Link{{Txid: head.TxHash(), Height: 10, BlockHash: *hash}},
		ScannedTo:   10,
		ScannedHash: *hash,
	}
	c.reads = nil
	if err := NewSPVBackend(c, nil).Advance(context.Background(), []*State{state}, noSave, nil); err != nil {
		t.Fatal(err)
	}
	if c.reads[*hash] != 1 || len(c.reads) != 1 {
		t.Fatalf("block read %d times, %d blocks", c.reads[*hash], len(c.reads))
	}
	if state.Head() == nil || state.Head().Txid != next.TxHash() || state.Status != StatusVerified {
		t.Fatalf("%+v", state)
	}
}

// TestSPVRegularFiltersUseOutpointSearches ensures regular filters never ask
// for a script-filter search. A token that is found, followed, and whose
// authbase is read uses SearchSpends and SearchOutputs only.
func TestSPVRegularFiltersUseOutpointSearches(t *testing.T) {
	c := newFakeChain(100)
	if c.ScriptFilters() {
		t.Fatal("regular filters report script filters")
	}
	authbase, genesis, category := p2pkhAuthbase(t, 1, true)
	next := spendOf(2, genesis.TxHash())
	c.put(75, authbase)
	c.put(80, genesis)
	c.put(90, next)
	state := resolveToken(t, c, category)
	if state.Head() == nil || state.Head().Txid != next.TxHash() {
		t.Fatalf("%+v", state)
	}
	if pub := state.CurrentPublication(); pub == nil || pub.URIs[0] != "authbase.example" {
		t.Fatalf("publication %+v", pub)
	}
	if scriptSpendCalls[c] != 0 || categoryOutputCalls[c] != 0 {
		t.Fatalf("script spend calls %d, category calls %d", scriptSpendCalls[c], categoryOutputCalls[c])
	}
	if len(c.searches) == 0 || len(c.outputSearches) == 0 {
		t.Fatalf("searches %v, output searches %v", c.searches, c.outputSearches)
	}
}
