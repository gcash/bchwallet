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
	GetBlock(hash *chainhash.Hash, quit <-chan struct{}) (*wire.MsgBlock, error)
	SearchSpends(outpoints []wire.OutPoint, from, to int32,
		found func(SpendFound) []wire.OutPoint, progress func(height int32),
		quit <-chan struct{}) (int32, error)
	SearchOutputs(scripts [][]byte, from, to int32, found func(OutputFound) bool,
		quit <-chan struct{}) (int32, error)
}

// OutputFound is a transaction a search for output scripts found.
type OutputFound struct {
	Tx        *wire.MsgTx
	Height    int32
	BlockHash chainhash.Hash
}

// authbaseReach is how far before its genesis a token's authbase is looked for,
// a week, which finds it 93% of the time, the genesis block itself 60%.
const authbaseReach = 1008

// SpendFound is a spend a search found.
type SpendFound struct {
	OutPoint  wire.OutPoint
	Tx        *wire.MsgTx
	Height    int32
	BlockHash chainhash.Hash
}

// maxNewBlockSearches bounds the passes over blocks that came during a search.
const maxNewBlockSearches = 5

// SPVBackend finds and follows identities over the P2P layer alone, with block
// filters.  The hints table says where each token stood at its height, so no
// scan ever goes below that height: a token in the table starts from its entry,
// and one that isn't was minted after it.
type SPVBackend struct {
	source spvSource
	hints  *chainbcmr.Hints

	// waiting holds when a token's start failed, and searchTried each failed
	// authbase search's tries, for the run goroutine.
	waiting     map[chainhash.Hash]time.Time
	searchTried map[chainhash.Hash]retry
}

// NewSPVBackend returns a backend.  Hints can be nil on a network without a
// table, where a search goes down to the first block.
func NewSPVBackend(source spvSource, hints *chainbcmr.Hints) *SPVBackend {
	return &SPVBackend{source: source, hints: hints, waiting: make(map[chainhash.Hash]time.Time),
		searchTried: make(map[chainhash.Hash]retry)}
}

// floor returns the height no scan goes below or to.
func (b *SPVBackend) floor() int32 {
	if b.hints == nil {
		return 0
	}
	return b.hints.Height
}

// RolledBack sets back what the state's scans covered above the height, as
// neutrino rolled back the filter headers to it, having found them wrong above
// it, so that the filters of those blocks, which could have hidden a spend or a
// genesis, are read again.  The hints table vouches for what's below its height.
// It returns whether the state changed.
func (b *SPVBackend) RolledBack(state *State, height int32) (bool, error) {
	height = max(height, b.floor())
	changed := false
	if state.ScannedTo > height {
		hash, err := b.source.GetBlockHash(int64(height))
		if err != nil {
			return false, err
		}
		state.ScannedTo, state.ScannedHash = height, *hash
		changed = true
	}

	// An authbase search that found nothing reached into those blocks.
	if state.AuthbaseSearched && len(state.AuthbaseScripts) > 0 && state.GenesisHeight-1 > height {
		state.AuthbaseSearched = false
		changed = true
	}
	return changed, nil
}

// Prepare gives each state that has no links its starting point: the table's
// entry, or the genesis from a search down to just above the table's height.  A
// token that can't be read waits and doesn't hold up the others.
func (b *SPVBackend) Prepare(ctx context.Context, states []*State) error {
	var search []*State
	for _, state := range states {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(state.Links) != 0 {
			continue
		}
		if last, ok := b.waiting[state.Category]; ok && time.Since(last) < retryInterval {
			continue
		}
		// A token in the table starts from it, so one that failed and is in
		// it now is in a newer table, which it starts from.
		if b.hints != nil {
			if hint, ok := b.hints.Lookup(state.Category); ok {
				if err := b.startFromHint(ctx, state, &hint); err != nil {
					log.Debugf("Unable to start token %v: %v", state.Category, err)
					b.waiting[state.Category] = time.Now()
				}
				continue
			}
		}
		if state.Status == StatusFailed {
			continue
		}
		search = append(search, state)
	}
	if len(search) == 0 {
		return nil
	}

	// A search that fails holds up only the tokens it's for, which try again
	// later, and the others go on.
	if err := b.searchGeneses(ctx, search); err != nil {
		if ctx.Err() != nil {
			return err
		}
		log.Warnf("Unable to search for token geneses: %v", err)
		for _, state := range search {
			b.waiting[state.Category] = time.Now()
		}
	}
	return nil
}

// startFromHint sets a state from the table.  Everything is read first, so that
// a block that can't be read leaves the state to try again rather than start
// without its publication.
func (b *SPVBackend) startFromHint(ctx context.Context, state *State, hint *chainbcmr.Hint) error {
	hash, err := b.source.GetBlockHash(int64(hint.HeadHeight))
	if err != nil {
		return err
	}
	head, err := b.readLink(ctx, hint.Head, hint.HeadHeight, hash)
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
		older, err = b.readLink(ctx, hint.Publication.Txid, hint.Publication.Height, pubHash)
		if err != nil {
			return err
		}
	}

	state.Links = []Link{*head}
	state.Burned = hint.Burned
	state.ScannedTo, state.ScannedHash = b.hints.Height, *scanned
	state.Status = StatusSearching
	if older != nil {
		state.Older = older.Publication
	}
	return nil
}

// readLink reads a transaction from its block, with its publication.
func (b *SPVBackend) readLink(ctx context.Context, txid chainhash.Hash, height int32,
	hash *chainhash.Hash) (*Link, error) {

	block, err := b.source.GetBlock(hash, ctx.Done())
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
	tipHash, tip, err := b.source.GetBestBlock()
	if err != nil {
		return err
	}
	bottom := b.floor() + 1
	if tip < bottom {
		return nil
	}

	// unknown holds the tokens whose genesis was found but couldn't be used.
	unknown := make(map[chainhash.Hash]struct{})
	outpoints, found := b.genesisFinder(ctx, states, unknown)
	if _, err = b.source.SearchSpends(outpoints, tip, bottom, found, nil, ctx.Done()); err != nil {
		return err
	}
	covered, coveredHash, same, err := b.searchNewBlocks(ctx, states, tip, tipHash, found)
	if err != nil {
		return err
	}
	for _, state := range states {
		if len(state.Links) != 0 {
			continue
		}
		if _, ok := unknown[state.Category]; ok || !same {
			b.waiting[state.Category] = time.Now()
			continue
		}
		state.Status = StatusFailed
		state.ScannedTo, state.ScannedHash = covered, *coveredHash
	}
	return nil
}

// genesisFinder returns the outpoints whose spends are the states' geneses, and
// what takes one up.  A genesis that can't be used goes in unknown.
func (b *SPVBackend) genesisFinder(ctx context.Context, states []*State,
	unknown map[chainhash.Hash]struct{}) ([]wire.OutPoint, func(SpendFound) []wire.OutPoint) {

	byOutPoint := make(map[wire.OutPoint]*State, len(states))
	outpoints := make([]wire.OutPoint, 0, len(states))
	for _, state := range states {
		outpoint := wire.OutPoint{Hash: state.Category}
		byOutPoint[outpoint] = state
		outpoints = append(outpoints, outpoint)
	}
	return outpoints, func(spend SpendFound) []wire.OutPoint {
		state := byOutPoint[spend.OutPoint]
		if !createsCategory(spend.Tx, state.Category) {
			return nil
		}
		// The next link can be in the same block, so the scan up starts
		// with that block.
		before, err := b.source.GetBlockHash(int64(spend.Height) - 1)
		if err != nil {
			log.Debugf("Unable to read the block before %v: %v", spend.BlockHash, err)
			unknown[state.Category] = struct{}{}
			return nil
		}
		// The block comes first, as a token with a link is never searched again,
		// and this is the only look at its authbase.
		block, err := b.source.GetBlock(&spend.BlockHash, ctx.Done())
		if err != nil {
			log.Debugf("Unable to read block %v: %v", spend.BlockHash, err)
			unknown[state.Category] = struct{}{}
			return nil
		}
		state.Links = []Link{*newLink(spend.Tx, spend.Height, spend.BlockHash)}
		state.Burned = chainbcmr.IsBurn(spend.Tx)
		state.ScannedTo, state.ScannedHash = spend.Height-1, *before
		state.Status = StatusSearching
		state.AuthbaseScripts, state.GenesisHeight = nil, spend.Height
		state.AuthbaseSearched = keepAuthbase(state, block, spend)
		if !state.AuthbaseSearched {
			state.AuthbaseScripts = authbaseScripts(spend.Tx, state.Category)
			state.AuthbaseSearched = len(state.AuthbaseScripts) == 0
		}
		return nil
	}
}

// keepAuthbase keeps a publication on the authbase, if it's in the block of the
// genesis, and returns whether it's there.  Some tokens publish only there.
func keepAuthbase(state *State, block *wire.MsgBlock, spend SpendFound) bool {
	for _, tx := range block.Transactions {
		if tx.TxHash() != state.Category {
			continue
		}
		if link := newLink(tx, spend.Height, spend.BlockHash); link.Publication != nil {
			state.Older = link.Publication
		}
		return true
	}
	return false
}

// checkFailed looks for the genesis of tokens that weren't found in the blocks
// that came since, as one that was still unconfirmed comes in one of those.  They
// are few, and it comes after what's published has shown, as a wallet that was off
// for a while has many blocks to search.
func (b *SPVBackend) checkFailed(ctx context.Context, all []*State, save func(*State)) error {
	var states []*State
	for _, state := range all {
		if state.Status == StatusFailed && len(state.Links) == 0 {
			states = append(states, state)
		}
	}
	if len(states) == 0 {
		return nil
	}

	// What comes after this can take long, and what was searched stays searched.
	defer func() {
		for _, state := range states {
			save(state)
		}
	}()
	tipHash, tip, err := b.source.GetBestBlock()
	if err != nil {
		return err
	}
	from := tip + 1
	for _, state := range states {
		// A state from before this kept nothing, and one can be on blocks
		// that are gone.  Either starts again from the floor, by being
		// searched as new.
		on := state.ScannedTo != 0
		if on {
			if on, err = b.onChain(state.ScannedTo, state.ScannedHash); err != nil {
				return err
			}
		}
		if !on {
			state.Status = StatusSearching
			continue
		}
		from = min(from, state.ScannedTo+1)
	}
	if from > tip {
		return nil
	}
	unknown := make(map[chainhash.Hash]struct{})
	outpoints, found := b.genesisFinder(ctx, states, unknown)
	if _, err := b.source.SearchSpends(outpoints, from, tip, found, nil, ctx.Done()); err != nil {
		return err
	}
	for _, state := range states {
		// A genesis that was found but couldn't be used is looked for again.
		if _, ok := unknown[state.Category]; !ok && state.Status == StatusFailed {
			state.ScannedTo, state.ScannedHash = tip, *tipHash
		}
	}
	return nil
}

// searchNewBlocks searches the blocks that came during a search, for the states
// it didn't find, until no more come.  It returns the block the search reached,
// and whether it covers the chain as it stands: a chain that changed can have put
// a genesis where the search had already looked, so finding nothing says nothing.
func (b *SPVBackend) searchNewBlocks(ctx context.Context, states []*State, tip int32,
	tipHash *chainhash.Hash, found func(SpendFound) []wire.OutPoint) (int32, *chainhash.Hash, bool, error) {

	for range maxNewBlockSearches {
		nowHash, now, err := b.source.GetBestBlock()
		if err != nil {
			return tip, tipHash, false, err
		}
		if *nowHash == *tipHash {
			return tip, tipHash, true, nil
		}
		same, err := b.sameChain(tip, tipHash)
		if err != nil || !same || now < tip {
			return tip, tipHash, false, err
		}
		outpoints := make([]wire.OutPoint, 0, len(states))
		for _, state := range states {
			if len(state.Links) == 0 {
				outpoints = append(outpoints, wire.OutPoint{Hash: state.Category})
			}
		}
		if len(outpoints) != 0 {
			_, err = b.source.SearchSpends(outpoints, tip+1, now, found, nil, ctx.Done())
			if err != nil {
				return tip, tipHash, false, err
			}
		}
		tip, tipHash = now, nowHash
	}
	return tip, tipHash, false, nil
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

// Forget has a token that waits try again at the next cycle, as asking for it
// does, its authbase search too.
func (b *SPVBackend) Forget(category chainhash.Hash) {
	delete(b.waiting, category)
	delete(b.searchTried, category)
}

// Advance checks each state against the chain, then follows every head that can
// still move up to the tip, in one pass.  A head the pass doesn't see spent is
// proven unspent, and its state Verified.
func (b *SPVBackend) Advance(ctx context.Context, states []*State, save func(*State)) error {
	if err := b.checkFailed(ctx, states, save); err != nil {
		if ctx.Err() != nil {
			return err
		}
		log.Warnf("Unable to look for tokens in new blocks: %v", err)
	}

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
		return b.searchAuthbases(ctx, states, save)
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
	return b.searchAuthbases(ctx, states, save)
}

// searchAuthbases looks for the authbase of each token that the walk followed to
// its head without a publication, in the week before its genesis, as some tokens
// publish only there.  It's done once, unless it fails, as then the next cycle
// tries again.
func (b *SPVBackend) searchAuthbases(ctx context.Context, states []*State, save func(*State)) error {
	for _, state := range states {
		if state.Status != StatusVerified || state.AuthbaseSearched || len(state.AuthbaseScripts) == 0 ||
			state.CurrentPublication() != nil {
			continue
		}
		// A search that failed waits as a registry read does, as each
		// downloads up to a week of blocks.
		if tried, ok := b.searchTried[state.Category]; ok && time.Since(tried.last) < retryWait(tried.tries) {
			continue
		}
		from := state.GenesisHeight - 1
		to := max(state.GenesisHeight-authbaseReach, 1)
		_, err := b.source.SearchOutputs(state.AuthbaseScripts, from, to, func(found OutputFound) bool {
			if found.Tx.TxHash() != state.Category {
				return true
			}
			if link := newLink(found.Tx, found.Height, found.BlockHash); link.Publication != nil {
				state.Older = link.Publication
			}
			return false
		}, ctx.Done())
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			tried := b.searchTried[state.Category]
			b.searchTried[state.Category] = retry{time.Now(), tried.tries + 1}
			log.Debugf("Unable to look for the authbase of token %v: %v", state.Category, err)
			continue
		}
		delete(b.searchTried, state.Category)
		state.AuthbaseSearched = true
		save(state)
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

	// The hash to keep is read first, so that the chain found the same after
	// it is the one it's from.
	hash, err := b.source.GetBlockHash(int64(height))
	if err != nil {
		return err
	}
	same, err := b.sameChain(tip, tipHash)
	if err != nil || !same {
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
		state.Older = nil
		state.AuthbaseScripts, state.GenesisHeight, state.AuthbaseSearched = nil, 0, false
		delete(b.searchTried, state.Category)
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
