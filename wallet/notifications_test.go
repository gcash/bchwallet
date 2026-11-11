package wallet

import (
	"bytes"
	"testing"

	"github.com/gcash/bchd/bchec"
	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/walletdb"
)

// TestTxSummaryOtherOutputs checks that the summary of a transaction the wallet
// sends lists the outputs that don't pay the wallet, with the address each pays
// by its hash, and none for a script that pays no single address that way, the
// script of an OP_RETURN output alone, and how many inputs it has.
func TestTxSummaryOtherOutputs(t *testing.T) {
	w := newRegtestWallet(t)
	params := &chaincfg.RegressionNetParams
	change := walletScript(t, w)

	payee, err := bchutil.NewAddressPubKeyHash(make([]byte, 20), params)
	if err != nil {
		t.Fatal(err)
	}
	scriptHash, err := bchutil.NewAddressScriptHashFromHash(make([]byte, 20), params)
	if err != nil {
		t.Fatal(err)
	}
	scriptHash32, err := bchutil.NewAddressScriptHash32FromHash(make([]byte, 32), params)
	if err != nil {
		t.Fatal(err)
	}
	key, err := bchec.NewPrivateKey(bchec.S256())
	if err != nil {
		t.Fatal(err)
	}
	pubKey, err := bchutil.NewAddressPubKey(key.PubKey().SerializeCompressed(), params)
	if err != nil {
		t.Fatal(err)
	}
	payTo := func(addr bchutil.Address) []byte {
		script, err := txscript.PayToAddrScript(addr)
		if err != nil {
			t.Fatal(err)
		}
		return script
	}
	data, err := txscript.NullDataScript([]byte("memo"))
	if err != nil {
		t.Fatal(err)
	}
	multisig, err := txscript.MultiSigScript([]*bchutil.AddressPubKey{pubKey}, 1)
	if err != nil {
		t.Fatal(err)
	}

	spent := addCredit(t, w, wire.NewTxOut(100000, change, wire.TokenData{}))
	tx := spendTx([]wire.OutPoint{spent},
		wire.NewTxOut(10000, payTo(payee), wire.TokenData{}),
		wire.NewTxOut(50000, change, wire.TokenData{}),
		wire.NewTxOut(0, data, wire.TokenData{}),
		wire.NewTxOut(2000, payTo(scriptHash), wire.TokenData{}),
		wire.NewTxOut(3000, payTo(scriptHash32), wire.TokenData{}),
		wire.NewTxOut(1000, payTo(payee), testToken),
		wire.NewTxOut(4000, multisig, wire.TokenData{}),
		wire.NewTxOut(5000, payTo(pubKey), wire.TokenData{}))
	rec := insertTx(t, w, tx, testBlock, 1)

	var summary TransactionSummary
	err = walletdb.View(w.db, func(dbtx walletdb.ReadTx) error {
		details, err := w.TxStore.TxDetails(dbtx.ReadBucket(wtxmgrNamespaceKey), &rec.Hash)
		if err != nil {
			return err
		}
		summary = makeTxSummary(dbtx, w, details)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(summary.MyOutputs) != 1 || summary.MyOutputs[0].Index != 1 {
		t.Errorf("wallet's outputs: got %+v, want the change at 1", summary.MyOutputs)
	}
	if summary.InputCount != 1 {
		t.Errorf("input count: got %d, want 1", summary.InputCount)
	}
	want := []struct {
		index    uint32
		address  bchutil.Address
		amount   bchutil.Amount
		token    wire.TokenData
		opReturn []byte
	}{
		{0, payee, 10000, wire.TokenData{}, nil},
		{2, nil, 0, wire.TokenData{}, data},
		{3, scriptHash, 2000, wire.TokenData{}, nil},
		{4, scriptHash32, 3000, wire.TokenData{}, nil},
		{5, payee, 1000, testToken, nil},
		{6, nil, 4000, wire.TokenData{}, nil},
		{7, nil, 5000, wire.TokenData{}, nil},
	}
	if len(summary.OtherOutputs) != len(want) {
		t.Fatalf("other outputs: got %d, want %d: %+v", len(summary.OtherOutputs),
			len(want), summary.OtherOutputs)
	}
	for i, w := range want {
		got := summary.OtherOutputs[i]
		if got.Index != w.index || got.Amount != w.amount ||
			!tokensEqual(&got.Token, &w.token) {

			t.Errorf("other output %d: got %+v, want index %d, amount %v",
				i, got, w.index, w.amount)
		}
		if !bytes.Equal(got.OpReturn, w.opReturn) {
			t.Errorf("output %d: got OP_RETURN %x, want %x", w.index,
				got.OpReturn, w.opReturn)
		}
		switch {
		case w.address == nil && got.Address != nil:
			t.Errorf("output %d: got address %v, want none", w.index, got.Address)
		case w.address != nil && (got.Address == nil ||
			got.Address.EncodeAddress() != w.address.EncodeAddress()):

			t.Errorf("output %d: got address %v, want %v", w.index, got.Address,
				w.address)
		}
	}
}

// TestGetTransactionsWithoutRaw checks that a read of the wallet's history
// without raw transactions leaves out only each transaction's serialized
// bytes, and that a read with them, as other clients make, still has them.
func TestGetTransactionsWithoutRaw(t *testing.T) {
	w := newRegtestWallet(t)
	spent := addCredit(t, w, wire.NewTxOut(100000, walletScript(t, w), wire.TokenData{}))
	tx := spendTx([]wire.OutPoint{spent},
		wire.NewTxOut(50000, walletScript(t, w), wire.TokenData{}))
	insertTx(t, w, tx, testBlock, 0)

	withRaw, err := w.GetTransactions(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	without, err := w.GetTransactionsWithoutRaw(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(withRaw.MinedTransactions) == 0 ||
		len(withRaw.MinedTransactions) != len(without.MinedTransactions) {

		t.Fatalf("got %d blocks with raw transactions and %d without",
			len(withRaw.MinedTransactions), len(without.MinedTransactions))
	}
	for i, block := range withRaw.MinedTransactions {
		other := without.MinedTransactions[i]
		if len(block.Transactions) != len(other.Transactions) {
			t.Fatalf("block %d: %d transactions with raw, %d without", i,
				len(block.Transactions), len(other.Transactions))
		}
		for j, summary := range block.Transactions {
			lean := other.Transactions[j]
			if len(summary.Transaction) == 0 {
				t.Errorf("block %d tx %d has no raw transaction", i, j)
			}
			if lean.Transaction != nil {
				t.Errorf("block %d tx %d has a raw transaction without", i, j)
			}
			if *lean.Hash != *summary.Hash || lean.Fee != summary.Fee ||
				len(lean.MyInputs) != len(summary.MyInputs) ||
				len(lean.MyOutputs) != len(summary.MyOutputs) {

				t.Errorf("block %d tx %d differs beyond its raw bytes", i, j)
			}
		}
	}
}
