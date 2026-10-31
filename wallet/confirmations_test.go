package wallet

import (
	"testing"
	"time"

	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchwallet/wallet/txrules"
	"github.com/gcash/bchwallet/wtxmgr"
)

// TestConfirmationsFromTip checks that the wallet counts confirmations, and
// the maturity of coinbase outputs, to the chain's tip rather than to the
// block it is synced to, which a rescan moves back, so that while a rescan
// runs the outputs mined above that block are in the balance and can be spent.
func TestConfirmationsFromTip(t *testing.T) {
	// The chain's tip is at height 200, and the wallet is synced to block
	// 10, as during a rescan from block 10.
	w := newRescanningWallet(t)
	script := walletScript(t, w)
	insertTx(t, w, &wire.MsgTx{
		Version: wire.TxVersion,
		TxIn:    []*wire.TxIn{{}},
		TxOut:   []*wire.TxOut{wire.NewTxOut(100000, script, wire.TokenData{})},
	}, &wtxmgr.BlockMeta{
		Block: wtxmgr.Block{Hash: chainhash.Hash{150}, Height: 150},
		Time:  time.Unix(1700000000, 0),
	}, 0)

	// A coinbase output mined at height 50 has the 100 confirmations it
	// needs at the tip.
	addCoinbase(t, w, script, 70000, 50)

	bals, err := w.CalculateAccountBalances(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if bals.Spendable != 170000 || bals.ImmatureReward != 0 {
		t.Errorf("spendable %v and immature %v, want 170000 and 0 "+
			"satoshis", int64(bals.Spendable),
			int64(bals.ImmatureReward))
	}

	outputs, err := w.UnspentOutputs(OutputSelectionPolicy{
		Account:               0,
		RequiredConfirmations: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 2 {
		t.Errorf("UnspentOutputs returned %d outputs, want 2",
			len(outputs))
	}

	_, err = w.CreateUnsignedTx(0, []*wire.TxOut{
		wire.NewTxOut(150000, script, wire.TokenData{}),
	}, nil, 1, txrules.DefaultRelayFeePerKb)
	if err != nil {
		t.Errorf("CreateUnsignedTx: %v", err)
	}
}
