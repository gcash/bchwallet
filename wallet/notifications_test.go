package wallet

import (
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
// by its hash, and none for a script that pays no single address that way.
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
	want := []struct {
		index   uint32
		address bchutil.Address
		amount  bchutil.Amount
		token   wire.TokenData
	}{
		{0, payee, 10000, wire.TokenData{}},
		{2, nil, 0, wire.TokenData{}},
		{3, scriptHash, 2000, wire.TokenData{}},
		{4, scriptHash32, 3000, wire.TokenData{}},
		{5, payee, 1000, testToken},
		{6, nil, 4000, wire.TokenData{}},
		{7, nil, 5000, wire.TokenData{}},
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
