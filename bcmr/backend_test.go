package bcmr

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/gcash/bchd/btcjson"
	"github.com/gcash/bchd/chaincfg/chainhash"
)

// fakeNode answers from a map of identities.
type fakeNode struct {
	identities map[chainhash.Hash]*btcjson.GetBcmrIdentityResult
	err        error
}

func (n *fakeNode) GetBestBlock() (*chainhash.Hash, int32, error) {
	return &chainhash.Hash{0xaa}, 100, nil
}

func (n *fakeNode) GetBcmrIdentity(authbase *chainhash.Hash, _ bool, _ *chainhash.Hash,
	_ int) (*btcjson.GetBcmrIdentityResult, error) {

	if n.err != nil {
		return nil, n.err
	}
	result, ok := n.identities[*authbase]
	if !ok {
		return nil, &btcjson.RPCError{Code: btcjson.ErrRPCInvalidAddressOrKey, Message: "none"}
	}
	return result, nil
}

// TestFullNodeBackend ensures an answer becomes a verified state, with its
// publication on the head or on an older link.
func TestFullNodeBackend(t *testing.T) {
	hash := [32]byte{9}
	onHead, older, burned := chainhash.Hash{1}, chainhash.Hash{2}, chainhash.Hash{3}
	head := chainhash.Hash{4}
	publication := func(txid string) *btcjson.BcmrPublicationResult {
		return &btcjson.BcmrPublicationResult{Txid: txid, Hash: hex.EncodeToString(hash[:]),
			URIs: []string{"example.com"}}
	}
	node := &fakeNode{identities: map[chainhash.Hash]*btcjson.GetBcmrIdentityResult{
		onHead: {Status: "active", Head: btcjson.BcmrLinkResult{Txid: head.String(), Height: 50,
			BlockHash: (chainhash.Hash{5}).String()}, Publication: publication(head.String())},
		older: {Status: "active", Head: btcjson.BcmrLinkResult{Txid: head.String(), Height: 50},
			Publication: publication((chainhash.Hash{6}).String())},
		burned: {Status: "burned", Head: btcjson.BcmrLinkResult{Txid: head.String(), Height: 50}},
	}}
	states := []*State{{Category: onHead}, {Category: older}, {Category: burned}}
	if err := NewFullNodeBackend(node).Prepare(context.Background(), states); err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if state.Status != StatusVerified || state.ScannedTo != 100 ||
			state.Head().Txid != head || state.Head().Height != 50 {

			t.Fatalf("got %+v", state)
		}
	}
	if states[0].Head().Publication == nil || states[0].CurrentPublication().Hash != hash ||
		states[0].Head().BlockHash != (chainhash.Hash{5}) {

		t.Fatalf("publication on the head: %+v", states[0])
	}
	if states[1].Head().Publication != nil || states[1].CurrentPublication().Hash != hash {
		t.Fatalf("older publication: %+v", states[1])
	}
	if !states[2].Burned || states[2].CurrentPublication() != nil {
		t.Fatalf("burned: %+v", states[2])
	}
}

// TestFullNodeBackendNoIndex ensures a node without the index leaves the token
// Unavailable rather than failing, and one that doesn't know it Failed.
func TestFullNodeBackendErrors(t *testing.T) {
	state := &State{Category: chainhash.Hash{1}}
	node := &fakeNode{err: &btcjson.RPCError{Code: btcjson.ErrRPCMisc, Message: "BCMR index must be enabled"}}
	if err := NewFullNodeBackend(node).Prepare(context.Background(), []*State{state}); err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusUnavailable {
		t.Fatalf("got %+v", state)
	}

	node = &fakeNode{}
	if err := NewFullNodeBackend(node).Prepare(context.Background(), []*State{state}); err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusFailed {
		t.Fatalf("got %+v", state)
	}

	node = &fakeNode{err: errors.New("connection lost")}
	if err := NewFullNodeBackend(node).Prepare(context.Background(), []*State{state}); err == nil {
		t.Fatal("a lost connection was taken for an answer")
	}
}
