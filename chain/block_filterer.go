package chain

import (
	"bytes"

	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/waddrmgr"
)

// BlockFilterer is used to iteratively scan blocks for a set of addresses of
// interest. This is done by constructing reverse indexes mapping the
// addresses to a ScopedIndex, which permits the reconstruction of the exact
// child deriviation paths that reported matches.
//
// Once initialized, a BlockFilterer can be used to scan any number of blocks
// until a invocation of `FilterBlock` returns true. This allows the reverse
// indexes to be resused in the event that the set of addresses does not need to
// be altered. After a match is reported, a new BlockFilterer should be
// initialized with the updated set of addresses that include any new keys that
// are now within our look-ahead.
//
// We track internal and external addresses separately in order to conserve the
// amount of space occupied in memory. Specifically, the account and branch
// combined contribute only 1-bit of information when using the default scopes
// used by the wallet. Thus we can avoid storing an additional 64-bits per
// address of interest by not storing the full derivation paths, and instead
// opting to allow the caller to contextually infer the account (DefaultAccount)
// and branch (Internal or External).
type BlockFilterer struct {
	// Params specifies the chain params of the current network.
	Params *chaincfg.Params

	// ExReverseFilter holds a reverse index mapping an external address, by
	// its addressKey, to the scoped index from which it was derived.
	ExReverseFilter map[string]waddrmgr.ScopedIndex

	// InReverseFilter holds a reverse index mapping an internal address, by
	// its addressKey, to the scoped index from which it was derived.
	InReverseFilter map[string]waddrmgr.ScopedIndex

	// WathcedOutPoints is a global set of outpoints being tracked by the
	// wallet. This allows the block filterer to check for spends from an
	// outpoint we own.
	WatchedOutPoints map[wire.OutPoint]bchutil.Address

	// FoundExternal is a two-layer map recording the scope and index of
	// external addresses found in a single block.
	FoundExternal map[waddrmgr.KeyScope]map[uint32]struct{}

	// FoundInternal is a two-layer map recording the scope and index of
	// internal addresses found in a single block.
	FoundInternal map[waddrmgr.KeyScope]map[uint32]struct{}

	// FoundOutPoints is a set of outpoints found in a single block whose
	// address belongs to the wallet.
	FoundOutPoints map[wire.OutPoint]bchutil.Address

	// RelevantTxns records the transactions found in a particular block
	// that contained matches from an address in either ExReverseFilter or
	// InReverseFilter.
	RelevantTxns []*wire.MsgTx
}

// NewBlockFilterer constructs the reverse indexes for the current set of
// external and internal addresses that we are searching for, and is used to
// scan successive blocks for addresses of interest. A particular block filter
// can be reused until the first call from `FitlerBlock` returns true.
func NewBlockFilterer(params *chaincfg.Params,
	req *FilterBlocksRequest) *BlockFilterer {

	// A request with a prebuilt watch index has its reverse indexes and
	// outpoints, which the filterer only reads.
	var exReverseFilter, inReverseFilter map[string]waddrmgr.ScopedIndex
	watchedOutPoints := req.WatchedOutPoints
	if req.Watch != nil {
		exReverseFilter = req.Watch.ExReverse
		inReverseFilter = req.Watch.InReverse
		watchedOutPoints = req.Watch.OutPoints
	} else {
		// Construct a reverse index by address key for the requested
		// external addresses.
		exReverseFilter = make(map[string]waddrmgr.ScopedIndex, len(req.ExternalAddrs))
		for scopedIndex, addr := range req.ExternalAddrs {
			exReverseFilter[addressKey(addr)] = scopedIndex
		}

		// Construct a reverse index by address key for the requested
		// internal addresses.
		inReverseFilter = make(map[string]waddrmgr.ScopedIndex, len(req.InternalAddrs))
		for scopedIndex, addr := range req.InternalAddrs {
			inReverseFilter[addressKey(addr)] = scopedIndex
		}
	}

	foundExternal := make(map[waddrmgr.KeyScope]map[uint32]struct{})
	foundInternal := make(map[waddrmgr.KeyScope]map[uint32]struct{})
	foundOutPoints := make(map[wire.OutPoint]bchutil.Address)

	return &BlockFilterer{
		Params:           params,
		ExReverseFilter:  exReverseFilter,
		InReverseFilter:  inReverseFilter,
		WatchedOutPoints: watchedOutPoints,
		FoundExternal:    foundExternal,
		FoundInternal:    foundInternal,
		FoundOutPoints:   foundOutPoints,
	}
}

// WatchIndex is the addresses a recovery watches, with the output script each
// pays and the reverse indexes of the block filterer, built as addresses are
// added rather than for each request, as recovery makes one for each block it
// finds the wallet's addresses in, and building them anew for all the addresses
// a long history watches took most of its work.  Adding to it isn't safe while
// a request that has it is filtered.
type WatchIndex struct {
	// Scripts is the watch list a filter is matched against: the output
	// scripts of the addresses, external and internal, and for each
	// outpoint, the script it pays and the outpoint itself.
	Scripts [][]byte

	// ExReverse and InReverse map the external and internal addresses, by
	// their addressKey, to the scoped index each was derived from.
	ExReverse map[string]waddrmgr.ScopedIndex
	InReverse map[string]waddrmgr.ScopedIndex

	// OutPoints are the outpoints watched for spends, with the address
	// each pays.
	OutPoints map[wire.OutPoint]bchutil.Address
}

// NewWatchIndex returns a WatchIndex of no addresses or outpoints.
func NewWatchIndex() *WatchIndex {
	return &WatchIndex{
		ExReverse: make(map[string]waddrmgr.ScopedIndex),
		InReverse: make(map[string]waddrmgr.ScopedIndex),
		OutPoints: make(map[wire.OutPoint]bchutil.Address),
	}
}

// AddOutPoint adds an outpoint to watch for spends, which pays the address,
// unless it's there already.
func (w *WatchIndex) AddOutPoint(op wire.OutPoint, addr bchutil.Address) error {
	if _, ok := w.OutPoints[op]; ok {
		return nil
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := op.Serialize(&buf); err != nil {
		return err
	}
	w.OutPoints[op] = addr
	w.Scripts = append(w.Scripts, script, buf.Bytes())
	return nil
}

// Add adds an address, internal or external, derived from the scoped index,
// unless it's there already.
func (w *WatchIndex) Add(scopedIndex waddrmgr.ScopedIndex, addr bchutil.Address,
	internal bool) error {

	reverse := w.ExReverse
	if internal {
		reverse = w.InReverse
	}
	key := addressKey(addr)
	if _, ok := reverse[key]; ok {
		return nil
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return err
	}
	reverse[key] = scopedIndex
	w.Scripts = append(w.Scripts, script)
	return nil
}

// addressKey returns what the reverse filters index an address by: its kind and
// the hash it pays, for the addresses a wallet derives, which takes no encoding,
// or else its encoding.  Recovery builds the filters from every address it
// watches for each range of blocks it scans, and encoding each one as a
// cashaddr every time made a long recovery slow, and its memory churn.
func addressKey(addr bchutil.Address) string {
	switch a := addr.(type) {
	case *bchutil.AddressPubKeyHash:
		return "h" + string(a.ScriptAddress())
	case *bchutil.AddressScriptHash:
		return "s" + string(a.ScriptAddress())
	default:
		return "e" + addr.EncodeAddress()
	}
}

// checkFilterTx will check if the transaction is relevant and recursively loop through the
// inputs to double check transactions that are in the block but were already processed.
// This is necessary if the block is not sorted in topological order.
func (bf *BlockFilterer) checkFilterTx(tx *wire.MsgTx, inputs map[chainhash.Hash][]*wire.MsgTx) {
	if bf.FilterTx(tx) {
		bf.RelevantTxns = append(bf.RelevantTxns, tx)
		if dependentTxs, ok := inputs[tx.TxHash()]; ok {
			for _, dependentTx := range dependentTxs {
				bf.checkFilterTx(dependentTx, inputs)
			}
		}
	}
}

// FilterBlock parses all txns in the provided block, searching for any that
// contain addresses of interest in either the external or internal reverse
// filters. This method return true iff the block contains a non-zero number of
// addresses of interest, or a transaction in the block spends from outpoints
// controlled by the wallet.
func (bf *BlockFilterer) FilterBlock(block *wire.MsgBlock) bool {
	inputs := make(map[chainhash.Hash][]*wire.MsgTx)
	for _, tx := range block.Transactions {
		for _, in := range tx.TxIn {
			inputTxs := inputs[in.PreviousOutPoint.Hash]
			inputTxs = append(inputTxs, tx)
			inputs[in.PreviousOutPoint.Hash] = inputTxs
		}
		bf.checkFilterTx(tx, inputs)
	}

	bf.sortRelevantTxs()
	return len(bf.RelevantTxns) > 0
}

// FilterTx scans all txouts in the provided txn, testing to see if any found
// addresses match those contained within the external or internal reverse
// indexes. This method returns true iff the txn contains a non-zero number of
// addresses of interest, or the transaction spends from an outpoint that
// belongs to the wallet.
func (bf *BlockFilterer) FilterTx(tx *wire.MsgTx) bool {
	var isRelevant bool

	// First, check the inputs to this transaction to see if they spend any
	// inputs belonging to the wallet. In addition to checking
	// WatchedOutPoints, we also check FoundOutPoints, in case a txn spends
	// from an outpoint created in the same block.
	for _, in := range tx.TxIn {
		if _, ok := bf.WatchedOutPoints[in.PreviousOutPoint]; ok {
			isRelevant = true
		}
		if _, ok := bf.FoundOutPoints[in.PreviousOutPoint]; ok {
			isRelevant = true
		}
	}

	// Now, parse all of the outputs created by this transactions, and see
	// if they contain any addresses known the wallet using our reverse
	// indexes for both external and internal addresses. If a new output is
	// found, we will add the outpoint to our set of FoundOutPoints.
	for i, out := range tx.TxOut {
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(
			out.PkScript, bf.Params,
		)
		if err != nil {
			log.Warnf("Could not parse output script in %s:%d: %v",
				tx.TxHash(), i, err)
			continue
		}

		if !bf.FilterOutputAddrs(addrs) {
			continue
		}

		// If we've reached this point, then the output contains an
		// address of interest.
		isRelevant = true

		// Record the outpoint that containing the address in our set of
		// found outpoints, so that the caller can update its global
		// set of watched outpoints.
		outPoint := wire.OutPoint{
			Hash:  *bchutil.NewTx(tx).Hash(),
			Index: uint32(i),
		}

		bf.FoundOutPoints[outPoint] = addrs[0]
	}

	return isRelevant
}

// FilterOutputAddrs tests the set of addresses against the block filterer's
// external and internal reverse address indexes. If any are found, they are
// added to set of external and internal found addresses, respectively. This
// method returns true iff a non-zero number of the provided addresses are of
// interest.
func (bf *BlockFilterer) FilterOutputAddrs(addrs []bchutil.Address) bool {
	var isRelevant bool
	for _, addr := range addrs {
		key := addressKey(addr)
		if scopedIndex, ok := bf.ExReverseFilter[key]; ok {
			bf.foundExternal(scopedIndex)
			isRelevant = true
		}
		if scopedIndex, ok := bf.InReverseFilter[key]; ok {
			bf.foundInternal(scopedIndex)
			isRelevant = true
		}
	}

	return isRelevant
}

// foundExternal marks the scoped index as found within the block filterer's
// FoundExternal map. If this the first index found for a particular scope, the
// scope's second layer map will be initialized before marking the index.
func (bf *BlockFilterer) foundExternal(scopedIndex waddrmgr.ScopedIndex) {
	if _, ok := bf.FoundExternal[scopedIndex.Scope]; !ok {
		bf.FoundExternal[scopedIndex.Scope] = make(map[uint32]struct{})
	}
	bf.FoundExternal[scopedIndex.Scope][scopedIndex.Index] = struct{}{}
}

// foundInternal marks the scoped index as found within the block filterer's
// FoundInternal map. If this the first index found for a particular scope, the
// scope's second layer map will be initialized before marking the index.
func (bf *BlockFilterer) foundInternal(scopedIndex waddrmgr.ScopedIndex) {
	if _, ok := bf.FoundInternal[scopedIndex.Scope]; !ok {
		bf.FoundInternal[scopedIndex.Scope] = make(map[uint32]struct{})
	}
	bf.FoundInternal[scopedIndex.Scope][scopedIndex.Index] = struct{}{}
}

// sortRelevantTxs sorts the RelevantTxns slice into topological order.
// That is, the child transactions come after parents. The wallet requires
// transactions be processed in topological order.
func (bf *BlockFilterer) sortRelevantTxs() {
	bf.RelevantTxns = removeDups(bf.RelevantTxns)

	sorted := make([]*wire.MsgTx, 0, len(bf.RelevantTxns))
	txids := make(map[chainhash.Hash]struct{})
	outpoints := make(map[wire.OutPoint]struct{})

	for _, tx := range bf.RelevantTxns {
		for i := range tx.TxOut {
			op := wire.OutPoint{
				Hash:  tx.TxHash(),
				Index: uint32(i),
			}
			outpoints[op] = struct{}{}
		}
	}

	for len(sorted) < len(bf.RelevantTxns) {
		for _, tx := range bf.RelevantTxns {
			if _, ok := txids[tx.TxHash()]; ok {
				continue
			}
			foundParent := false
			for _, in := range tx.TxIn {
				if _, ok := outpoints[in.PreviousOutPoint]; ok {
					foundParent = true
					break
				}
			}
			if !foundParent {
				sorted = append(sorted, tx)
				for i := range tx.TxOut {
					op := wire.OutPoint{
						Hash:  tx.TxHash(),
						Index: uint32(i),
					}
					delete(outpoints, op)
				}
				txids[tx.TxHash()] = struct{}{}
			}
		}
	}
	bf.RelevantTxns = sorted
}

func removeDups(txs []*wire.MsgTx) []*wire.MsgTx {
	keys := make(map[*wire.MsgTx]bool)
	var ret []*wire.MsgTx
	for _, tx := range txs {
		if _, ok := keys[tx]; !ok {
			keys[tx] = true
			ret = append(ret, tx)
		}
	}
	return ret
}
