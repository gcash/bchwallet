package rpcserver

import (
	"testing"

	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/wallet"
)

// TestMarshalOtherOutputs checks that the outputs that don't pay the wallet are
// marshaled with their index, address, amount and tokens, and an output
// without an address with an empty one.
func TestMarshalOtherOutputs(t *testing.T) {
	payee, err := bchutil.NewAddressPubKeyHash(make([]byte, 20), &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	token := wire.TokenData{CategoryID: [32]byte{1}, Amount: 5, BitField: wire.HAS_AMOUNT}

	details := marshalTransactionDetails([]wallet.TransactionSummary{{
		Hash: &chainhash.Hash{},
		OtherOutputs: []wallet.TransactionSummaryOtherOutput{
			{Index: 0, Address: payee, Amount: 10000},
			{Index: 2},
			{Index: 3, Address: payee, Amount: 1000, Token: token},
		},
	}})
	others := details[0].OtherOutputs
	if len(others) != 3 {
		t.Fatalf("got %d other outputs, want 3: %v", len(others), others)
	}
	if others[0].Index != 0 || others[0].Address != payee.String() ||
		others[0].Amount != 10000 || others[0].Token != nil {

		t.Errorf("payment: got %v", others[0])
	}
	if others[1].Index != 2 || others[1].Address != "" || others[1].Amount != 0 {
		t.Errorf("output without an address: got %v", others[1])
	}
	if others[2].Index != 3 || others[2].Address != payee.String() ||
		others[2].Token == nil || others[2].Token.Amount != 5 {

		t.Errorf("payment with tokens: got %v", others[2])
	}
}
