package wallet

import (
	"testing"
	"time"

	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchwallet/wtxmgr"
)

// addCoinbase records a coinbase transaction mined at height that pays value
// to script, and returns the outpoint of its output.
func addCoinbase(t *testing.T, w *Wallet, script []byte, value int64,
	height int32) wire.OutPoint {

	t.Helper()

	rec := insertTx(t, w, &wire.MsgTx{
		Version: wire.TxVersion,
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: wire.OutPoint{
				Index: wire.MaxPrevOutIndex,
			},
			SignatureScript: []byte{0x01, byte(height), 0x51},
		}},
		TxOut: []*wire.TxOut{wire.NewTxOut(value, script, wire.TokenData{})},
	}, &wtxmgr.BlockMeta{
		Block: wtxmgr.Block{Hash: chainhash.Hash{byte(height)}, Height: height},
		Time:  time.Unix(1700000000, 0),
	}, 0)
	return wire.OutPoint{Hash: rec.Hash, Index: 0}
}

// TestUnspentOutputsImmatureCoinbase checks that UnspentOutputs leaves out
// coinbase outputs that haven't matured unless asked for them, as the network
// rejects a transaction that spends one.  SweepAccount and FundTransaction
// offered them.
func TestUnspentOutputsImmatureCoinbase(t *testing.T) {
	w := newRegtestWallet(t)
	script := walletScript(t, w)

	// A coinbase output matures once it has as many confirmations as the
	// coinbase maturity.
	maturity := int32(w.chainParams.CoinbaseMaturity)
	mature := addCoinbase(t, w, script, 5000, 1)
	immature := addCoinbase(t, w, script, 6000, 2)
	setSyncedTo(t, w, maturity)

	for _, test := range []struct {
		include bool
		want    []wire.OutPoint
	}{
		{false, []wire.OutPoint{mature}},
		{true, []wire.OutPoint{mature, immature}},
	} {
		outputs, err := w.UnspentOutputs(OutputSelectionPolicy{
			IncludeImmatureCoinbases: test.include,
		})
		if err != nil {
			t.Fatal(err)
		}

		got := make(map[wire.OutPoint]bool, len(outputs))
		for _, output := range outputs {
			got[output.OutPoint] = true
			if output.OutputKind != OutputKindCoinbase {
				t.Errorf("output %v isn't marked as a coinbase",
					output.OutPoint)
			}
		}
		if len(got) != len(test.want) {
			t.Fatalf("with immature coinbases %v: got %d outputs, "+
				"want %d", test.include, len(got), len(test.want))
		}
		for _, op := range test.want {
			if !got[op] {
				t.Fatalf("with immature coinbases %v: missing %v",
					test.include, op)
			}
		}
	}
}
