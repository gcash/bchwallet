package bcmr

import (
	"context"
	"fmt"
	"time"

	chainbcmr "github.com/gcash/bchd/bcmr"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/wire"
)

const (
	// maxReorgDepth is how far back a head's scan is redone after its last
	// scanned block left the chain.
	maxReorgDepth = 100

	// saveInterval is how many heights a scan covers between saves, so that
	// an interrupted one continues rather than starts over.
	saveInterval = 2000
)

// spvSource is what the SPV backend reads.
type spvSource interface {
	GetBestBlock() (*chainhash.Hash, int32, error)
	GetBlockHash(height int64) (*chainhash.Hash, error)
	GetBlock(hash *chainhash.Hash) (*wire.MsgBlock, error)
	SearchSpends(outpoints []wire.OutPoint, from, to int32,
		found func(SpendFound) []wire.OutPoint, progress func(height int32),
		quit <-chan struct{}) (int32, error)
}

// SpendFound is a spend a search found.
type SpendFound struct {
	OutPoint  wire.OutPoint
	Tx        *wire.MsgTx
	Height    int32
	BlockHash chainhash.Hash
}

// SPVBackend finds and follows identities over the P2P layer alone, with block
// filters.  The hints table says where each token stood at its height, so no
// scan ever goes below that height: a token in the table starts from its entry,
// and one that isn't was minted after it.
type SPVBackend struct {
	source spvSource
	hints  *chainbcmr.Hints

	// waiting holds when a token's start failed, for the run goroutine.
	waiting map[chainhash.Hash]time.Time
}

// NewSPVBackend returns a backend.  Hints can be nil on a network without a
// table, where a search goes down to the first block.
func NewSPVBackend(source spvSource, hints *chainbcmr.Hints) *SPVBackend {
	return &SPVBackend{source: source, hints: hints, waiting: make(map[chainhash.Hash]time.Time)}
}

// floor returns the height no scan goes below or to.
func (b *SPVBackend) floor() int32 {
	if b.hints == nil {
		return 0
	}
	return b.hints.Height
}

// Prepare gives each state that has no links its starting point: the table's
// entry, or the genesis from a search down to just above the table's height.  A
// token that can't be read waits and doesn't hold up the others, and a Failed one
// waits to be asked for again.
func (b *SPVBackend) Prepare(ctx context.Context, states []*State) error {
	var search []*State
	for _, state := range states {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(state.Links) != 0 || state.Status == StatusFailed {
			continue
		}
		if last, ok := b.waiting[state.Category]; ok && time.Since(last) < retryInterval {
			continue
		}
		if b.hints != nil {
			if hint, ok := b.hints.Lookup(state.Category); ok {
				if err := b.startFromHint(state, &hint); err != nil {
					log.Debugf("Unable to start token %v: %v", state.Category, err)
					b.waiting[state.Category] = time.Now()
				}
				continue
			}
		}
		search = append(search, state)
	}
	if len(search) == 0 {
		return nil
	}
	return b.searchGeneses(ctx, search)
}

// startFromHint sets a state from the table.  Everything is read first, so that
// a block that can't be read leaves the state to try again rather than start
// without its publication.
func (b *SPVBackend) startFromHint(state *State, hint *chainbcmr.Hint) error {
	hash, err := b.source.GetBlockHash(int64(hint.HeadHeight))
	if err != nil {
		return err
	}
	head, err := b.readLink(hint.Head, hint.HeadHeight, hash)
	if err != nil {
		return err
	}
	scanned, err := b.source.GetBlockHash(int64(b.hints.Height))
	if err != nil {
		return err
	}
	var older *Link
	if hint.Publication != nil {
		pubHash, err := b.source.GetBlockHash(int64(hint.Publication.Height))
		if err != nil {
			return err
		}
		older, err = b.readLink(hint.Publication.Txid, hint.Publication.Height, pubHash)
		if err != nil {
			return err
		}
	}

	state.Links = []Link{*head}
	state.Burned = hint.Burned
	state.ScannedTo, state.ScannedHash = b.hints.Height, *scanned
	state.Status = StatusSearching
	if older != nil {
		txid := older.Txid
		state.OlderTxid, state.OlderHeight = &txid, older.Height
		state.Older = older.Publication
	}
	return nil
}

// readLink reads a transaction from its block, with its publication.
func (b *SPVBackend) readLink(txid chainhash.Hash, height int32, hash *chainhash.Hash) (*Link, error) {
	block, err := b.source.GetBlock(hash)
	if err != nil {
		return nil, err
	}
	for _, tx := range block.Transactions {
		if tx.TxHash() == txid {
			return newLink(tx, height, *hash), nil
		}
	}
	return nil, fmt.Errorf("transaction %v isn't in block %v", txid, hash)
}

// newLink returns the link of a transaction.
func newLink(tx *wire.MsgTx, height int32, blockHash chainhash.Hash) *Link {
	link := &Link{Txid: tx.TxHash(), Height: height, BlockHash: blockHash}
	if pub := chainbcmr.ParsePublication(tx); pub != nil && pub.WellFormed() {
		link.Publication = &Publication{Hash: pub.Hash, URIs: pub.URIs}
	}
	return link
}

// searchGeneses finds the genesis of each token, down from the tip.  The hints
// mean it was minted above the table's height, so a genesis that isn't there
// leaves the token Failed rather than sending the search further down.
func (b *SPVBackend) searchGeneses(ctx context.Context, states []*State) error {
	_, tip, err := b.source.GetBestBlock()
	if err != nil {
		return err
	}
	bottom := b.floor() + 1
	if tip < bottom {
		return nil
	}

	byOutPoint := make(map[wire.OutPoint]*State, len(states))
	outpoints := make([]wire.OutPoint, 0, len(states))
	for _, state := range states {
		outpoint := wire.OutPoint{Hash: state.Category}
		byOutPoint[outpoint] = state
		outpoints = append(outpoints, outpoint)
	}
	_, err = b.source.SearchSpends(outpoints, tip, bottom, func(spend SpendFound) []wire.OutPoint {
		state := byOutPoint[spend.OutPoint]
		if !createsCategory(spend.Tx, state.Category) {
			return nil
		}
		// The next link can be in the same block, so the scan up starts
		// with that block.
		before, err := b.source.GetBlockHash(int64(spend.Height) - 1)
		if err != nil {
			log.Debugf("Unable to read the block before %v: %v", spend.BlockHash, err)
			return nil
		}
		state.Links = []Link{*newLink(spend.Tx, spend.Height, spend.BlockHash)}
		state.Burned = chainbcmr.IsBurn(spend.Tx)
		state.ScannedTo, state.ScannedHash = spend.Height-1, *before
		state.Status = StatusSearching
		return nil
	}, nil, ctx.Done())
	if err != nil {
		return err
	}
	for _, state := range states {
		if len(state.Links) == 0 {
			state.Status = StatusFailed
		}
	}
	return nil
}

// createsCategory returns whether tx is the genesis of the category.
func createsCategory(tx *wire.MsgTx, category chainhash.Hash) bool {
	for _, genesis := range chainbcmr.Geneses(tx) {
		if genesis.Category == category {
			return true
		}
	}
	return false
}

// Advance checks each state against the chain, then follows every head that can
// still move up to the tip, in one pass.  A head the pass doesn't see spent is
// proven unspent, and its state Verified.
func (b *SPVBackend) Advance(ctx context.Context, states []*State, save func(*State)) error {
	// The tip comes first, so that a reorg from here on shows as a changed chain.
	tipHash, tip, err := b.source.GetBestBlock()
	if err != nil {
		return err
	}
	for _, state := range states {
		if err := b.rewind(state); err != nil {
			return err
		}
	}

	from := tip + 1
	watching := make(map[wire.OutPoint][]*State)
	for _, state := range states {
		head := state.Head()
		if head == nil || state.Status == StatusFailed {
			continue
		}
		if state.Burned {
			if state.Status != StatusVerified {
				state.Status = StatusVerified
				save(state)
			}
			continue
		}
		outpoint := wire.OutPoint{Hash: head.Txid}
		watching[outpoint] = append(watching[outpoint], state)
		from = min(from, state.ScannedTo+1)
	}
	if len(watching) == 0 {
		return nil
	}

	active := make([]*State, 0, len(states))
	for _, followers := range watching {
		active = append(active, followers...)
	}
	if from <= tip {
		outpoints := make([]wire.OutPoint, 0, len(watching))
		for outpoint := range watching {
			outpoints = append(outpoints, outpoint)
		}
		lastSave := from

		// spent holds the spends found in the block being read.  Chains
		// that merge share what follows, and one can get there after the
		// other has gone further, when they merge in a block where the
		// links go on, so it follows the rest from here.  A later block
		// can't hold what an earlier one spent, and a chain through a
		// busy contract has any number of links, so it keeps one block's.
		spent := make(map[wire.OutPoint]SpendFound)
		var spentHeight int32
		follow := func(spend SpendFound) []wire.OutPoint {
			if spend.Height != spentHeight {
				clear(spent)
				spentHeight = spend.Height
			}
			spent[spend.OutPoint] = spend
			followers := watching[spend.OutPoint]
			delete(watching, spend.OutPoint)
			for {
				link := newLink(spend.Tx, spend.Height, spend.BlockHash)
				burned := chainbcmr.IsBurn(spend.Tx)
				for _, state := range followers {
					state.Links = append(state.Links, *link)
					state.trimLinks()
					state.Burned = burned
					if burned {
						state.ScannedTo, state.ScannedHash = spend.Height, spend.BlockHash
						save(state)
					}
				}
				if burned {
					return nil
				}
				next := wire.OutPoint{Hash: link.Txid}
				later, ok := spent[next]
				if !ok {
					watching[next] = append(watching[next], followers...)
					return []wire.OutPoint{next}
				}
				spend = later
			}
		}
		progress := func(height int32) {
			if height-lastSave < saveInterval {
				return
			}
			lastSave = height
			if err := b.markScanned(active, height, tip, tipHash, save); err != nil {
				log.Warnf("Unable to save a token search: %v", err)
			}
		}
		reached, err := b.source.SearchSpends(outpoints, from, tip, follow, progress, ctx.Done())
		if err != nil {
			if reached >= from {
				_ = b.markScanned(active, reached, tip, tipHash, save)
			}
			return err
		}
	}
	same, err := b.sameChain(tip, tipHash)
	if err != nil {
		return err
	}
	if !same {
		return nil
	}
	if err := b.markScanned(active, tip, tip, tipHash, save); err != nil {
		return err
	}
	for _, state := range active {
		if state.Status != StatusVerified {
			state.Status = StatusVerified
			save(state)
		}
	}
	return nil
}

// sameChain returns whether the block at height is still hash, so that what was
// scanned up to it is still the chain.
func (b *SPVBackend) sameChain(height int32, hash *chainhash.Hash) (bool, error) {
	current, err := b.source.GetBlockHash(int64(height))
	if err != nil {
		return false, err
	}
	return *current == *hash, nil
}

// markScanned records that the heads of the states were unspent up to height.  A
// chain that changed since the scan began to tip leaves them as they were, for
// the next pass to go back from: the blocks it read may not be the ones there
// now, and the old hash is what tells so.
func (b *SPVBackend) markScanned(states []*State, height, tip int32, tipHash *chainhash.Hash,
	save func(*State)) error {

	same, err := b.sameChain(tip, tipHash)
	if err != nil || !same {
		return err
	}
	hash, err := b.source.GetBlockHash(int64(height))
	if err != nil {
		return err
	}
	for _, state := range states {
		if !state.Burned && state.ScannedTo < height {
			state.ScannedTo, state.ScannedHash = height, *hash
			save(state)
		}
	}
	return nil
}

// onChain returns whether the block is at its height in the chain now.  An
// error means the chain couldn't be asked, not that the block left it.
func (b *SPVBackend) onChain(height int32, hash chainhash.Hash) (bool, error) {
	_, tip, err := b.source.GetBestBlock()
	if err != nil {
		return false, err
	}
	if height > tip {
		return false, nil
	}
	current, err := b.source.GetBlockHash(int64(height))
	if err != nil {
		return false, err
	}
	return *current == hash, nil
}

// rewind drops what a reorganization took off the chain: any link whose block
// left it, and the scan above where it could have changed.  Links can be above
// the scanned height, so both are looked at.  A state with no links left starts
// again.
func (b *SPVBackend) rewind(state *State) error {
	head := state.Head()
	if head == nil {
		return nil
	}
	headOn, err := b.onChain(head.Height, head.BlockHash)
	if err != nil {
		return err
	}
	scannedOn, err := b.onChain(state.ScannedTo, state.ScannedHash)
	if err != nil {
		return err
	}
	if headOn && scannedOn {
		return nil
	}

	kept := len(state.Links)
	for kept > 0 {
		on, err := b.onChain(state.Links[kept-1].Height, state.Links[kept-1].BlockHash)
		if err != nil {
			return err
		}
		if on {
			break
		}
		kept--
	}
	if kept < len(state.Links) {
		state.Burned = false
	}
	state.Links = state.Links[:kept]
	state.Status = StatusSearching
	head = state.Head()
	if head == nil {
		state.ScannedTo, state.ScannedHash = 0, chainhash.Hash{}
		state.OlderTxid, state.OlderHeight, state.Older = nil, 0, nil
		return nil
	}

	// A scanned block that left the chain says only that a block near it did.
	height := max(state.ScannedTo-maxReorgDepth, head.Height, b.floor())
	hash, err := b.source.GetBlockHash(int64(height))
	if err != nil {
		return err
	}
	state.ScannedTo, state.ScannedHash = height, *hash
	return nil
}
