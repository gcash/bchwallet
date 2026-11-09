package bcmr

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/gcash/bchd/btcjson"
	"github.com/gcash/bchd/chaincfg/chainhash"
)

// Backend finds and follows identities.
type Backend interface {
	// Prepare finds where each state starts, so that what's published so
	// far can be shown while the rest takes its time.
	Prepare(ctx context.Context, states []*State) error

	// Advance follows the identities to the tip, and calls save whenever a
	// state changed.
	Advance(ctx context.Context, states []*State, save func(*State)) error
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
}

// NewFullNodeBackend returns a backend on the node.
func NewFullNodeBackend(node bcmrNode) *FullNodeBackend {
	return &FullNodeBackend{node: node}
}

// Prepare reads each identity.  The answer is the whole of it, so it's Verified
// at once.  A node without the index leaves the states Unavailable, and an
// identity it doesn't know Failed.
func (b *FullNodeBackend) Prepare(ctx context.Context, states []*State) error {
	hash, height, err := b.node.GetBestBlock()
	if err != nil {
		return err
	}
	for _, state := range states {
		if err := ctx.Err(); err != nil {
			return err
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
		case err != nil:
			return err
		}
		if err := applyIdentity(state, result); err != nil {
			return err
		}
		state.ScannedTo, state.ScannedHash = height, *hash
		state.Status = StatusVerified
	}
	return nil
}

// Advance has nothing to do, as Prepare reads the node's whole answer.
func (b *FullNodeBackend) Advance(context.Context, []*State, func(*State)) error {
	return nil
}

// applyIdentity sets a state from the node's answer.  A head in the node's
// mempool has no height, which is fine here, as the node handles reorgs.
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

	state.Links = []Link{link}
	state.Older = nil
	state.Burned = result.Status == "burned"
	if result.Publication == nil {
		return nil
	}
	raw, err := hex.DecodeString(result.Publication.Hash)
	if err != nil || len(raw) != 32 {
		return errors.New("invalid publication hash")
	}
	pub := &Publication{URIs: result.Publication.URIs}
	copy(pub.Hash[:], raw)
	if result.Publication.Txid == result.Head.Txid {
		state.Links[0].Publication = pub
	} else {
		state.Older = pub
	}
	return nil
}
