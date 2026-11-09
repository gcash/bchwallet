package bcmr

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"github.com/gcash/bchd/btcjson"
	"github.com/gcash/bchd/chaincfg/chainhash"
)

// Backend finds and follows identities.
type Backend interface {
	// Prepare finds where each state starts, so that what's published so
	// far can be shown while the rest takes its time.
	Prepare(ctx context.Context, states []*State) error

	// Advance follows the identities to the tip, and calls save whenever a
	// state changed, and between, if it isn't nil, every few seconds of a
	// long scan, so that what's asked for meanwhile needn't wait for it.
	Advance(ctx context.Context, states []*State, save func(*State), between func()) error
}

// bcmrNode is what the full node backend asks of the node.
type bcmrNode interface {
	GetBestBlock() (*chainhash.Hash, int32, error)
	GetBcmrIdentity(authbase *chainhash.Hash, includeAuthchain bool, after *chainhash.Hash,
		limit int) (*btcjson.GetBcmrIdentityResult, error)
}

// FullNodeBackend reads identities from the BCMR index of the node the wallet
// already trusts for everything else, so it needs no scanning of its own.
type FullNodeBackend struct {
	node bcmrNode

	// waiting holds when the node's answer for a token was an error it
	// didn't expect, and again the tokens asked for again, for the run
	// goroutine.
	waiting map[chainhash.Hash]time.Time
	again   map[chainhash.Hash]struct{}
}

// NewFullNodeBackend returns a backend on the node.
func NewFullNodeBackend(node bcmrNode) *FullNodeBackend {
	return &FullNodeBackend{node: node, waiting: make(map[chainhash.Hash]time.Time),
		again: make(map[chainhash.Hash]struct{})}
}

// Forget has a token that waits try again at the next cycle, as asking for it
// does, and one read at the tip read again.
func (b *FullNodeBackend) Forget(category chainhash.Hash) {
	delete(b.waiting, category)
	b.again[category] = struct{}{}
}

// Prepare reads each identity.  The answer is the whole of it, so it's Verified
// at once.  A node without the index leaves the states Unavailable, and an
// identity it doesn't know Failed.  Any other error the node answers for a
// token, or an answer that can't be used, has that token wait as a registry
// read does, and the others go on.  A token read at the tip isn't read again
// until the next block, unless it's asked for again.
func (b *FullNodeBackend) Prepare(ctx context.Context, states []*State) error {
	hash, height, err := b.node.GetBestBlock()
	if err != nil {
		return err
	}
	for _, state := range states {
		if err := ctx.Err(); err != nil {
			return err
		}
		if last, ok := b.waiting[state.Category]; ok && time.Since(last) < retryInterval {
			continue
		}
		if _, again := b.again[state.Category]; !again && state.Status == StatusVerified &&
			state.ScannedHash == *hash {

			continue
		}
		result, err := b.node.GetBcmrIdentity(&state.Category, false, nil, 0)
		var rpcErr *btcjson.RPCError
		switch {
		case errors.As(err, &rpcErr) && (rpcErr.Code == btcjson.ErrRPCMisc ||
			rpcErr.Code == btcjson.ErrRPCMethodNotFound.Code):

			state.Status = StatusUnavailable
			continue
		case errors.As(err, &rpcErr) && rpcErr.Code == btcjson.ErrRPCInvalidAddressOrKey:
			state.Status = StatusFailed
			continue
		case errors.As(err, &rpcErr):
			b.wait(state.Category, err)
			continue
		case err != nil:
			return err
		}
		if err := applyIdentity(state, result); err != nil {
			b.wait(state.Category, err)
			continue
		}
		delete(b.waiting, state.Category)
		state.ScannedTo, state.ScannedHash = height, *hash
		state.Status = StatusVerified
	}
	// Every token asked for again was in this pass.
	clear(b.again)
	return nil
}

// wait has a token whose answer was an error wait before it's asked for again.
func (b *FullNodeBackend) wait(category chainhash.Hash, err error) {
	log.Debugf("Unable to read the identity of token %v: %v", category, err)
	b.waiting[category] = time.Now()
}

// Advance has nothing to do, as Prepare reads the node's whole answer.
func (b *FullNodeBackend) Advance(context.Context, []*State, func(*State), func()) error {
	return nil
}

// applyIdentity sets a state from the node's answer, or leaves it as it was if
// the answer can't be used.  A head in the node's mempool has no height, which
// is fine here, as the node handles reorgs.
func applyIdentity(state *State, result *btcjson.GetBcmrIdentityResult) error {
	head, err := chainhash.NewHashFromStr(result.Head.Txid)
	if err != nil {
		return err
	}
	link := Link{Txid: *head, Height: result.Head.Height}
	if result.Head.BlockHash != "" {
		blockHash, err := chainhash.NewHashFromStr(result.Head.BlockHash)
		if err != nil {
			return err
		}
		link.BlockHash = *blockHash
	}
	var pub *Publication
	if result.Publication != nil {
		raw, err := hex.DecodeString(result.Publication.Hash)
		if err != nil || len(raw) != 32 {
			return errors.New("invalid publication hash")
		}
		pub = &Publication{URIs: result.Publication.URIs}
		copy(pub.Hash[:], raw)
	}

	state.Links = []Link{link}
	state.Older = nil
	state.Burned = result.Status == "burned"
	if pub == nil {
		return nil
	}
	if result.Publication.Txid == result.Head.Txid {
		state.Links[0].Publication = pub
	} else {
		state.Older = pub
	}
	return nil
}
