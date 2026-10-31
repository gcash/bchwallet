package rpcserver

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil/hdkeychain"
	pb "github.com/gcash/bchwallet/rpc/walletrpc"
	"github.com/gcash/bchwallet/waddrmgr"
	"github.com/gcash/bchwallet/wallet"
	"github.com/gcash/bchwallet/walletdb"
	_ "github.com/gcash/bchwallet/walletdb/bdb"
	"github.com/gcash/bchwallet/wtxmgr"
)

// The names of the wallet's database namespaces, which the wallet package
// doesn't export.
var (
	addrmgrNamespace = []byte("waddrmgr")
	txmgrNamespace   = []byte("wtxmgr")
)

// TestSweepSkipsImmatureCoinbase checks that SweepAccount leaves out coinbase
// outputs that haven't matured, and that FundTransaction does unless
// include_immature_coinbases is set.  The network rejects a transaction that
// spends one, so the sweep failed, and FundTransaction ignored the field.
func TestSweepSkipsImmatureCoinbase(t *testing.T) {
	seed, err := hdkeychain.GenerateSeed(hdkeychain.MinSeedBytes)
	if err != nil {
		t.Fatal(err)
	}
	params := &chaincfg.RegressionNetParams
	loader := wallet.NewLoader(params, t.TempDir(), true, 250)
	w, err := loader.CreateNewWallet([]byte("public"), []byte("private"),
		seed, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := loader.UnloadWallet(); err != nil {
			t.Error(err)
		}
	}()

	addr, err := w.CurrentAddress(0, waddrmgr.KeyScopeBIP0044)
	if err != nil {
		t.Fatal(err)
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatal(err)
	}

	// Record a coinbase output that has matured and one that hasn't.
	maturity := int32(params.CoinbaseMaturity)
	coinbase := func(height int32) wire.OutPoint {
		tx := &wire.MsgTx{
			Version: wire.TxVersion,
			TxIn: []*wire.TxIn{{
				PreviousOutPoint: wire.OutPoint{
					Index: wire.MaxPrevOutIndex,
				},
				SignatureScript: []byte{0x01, byte(height), 0x51},
			}},
			TxOut: []*wire.TxOut{
				wire.NewTxOut(100000, script, wire.TokenData{}),
			},
		}
		rec, err := wtxmgr.NewTxRecordFromMsgTx(tx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		block := &wtxmgr.BlockMeta{
			Block: wtxmgr.Block{
				Hash:   chainhash.Hash{byte(height)},
				Height: height,
			},
			Time: time.Unix(1700000000, 0),
		}
		err = walletdb.Update(w.Database(), func(dbtx walletdb.ReadWriteTx) error {
			ns := dbtx.ReadWriteBucket(txmgrNamespace)
			if err := w.TxStore.InsertTx(ns, rec, block); err != nil {
				return err
			}
			return w.TxStore.AddCredit(ns, rec, block, 0, false)
		})
		if err != nil {
			t.Fatal(err)
		}
		return wire.OutPoint{Hash: rec.Hash, Index: 0}
	}
	mature := coinbase(1)
	immature := coinbase(2)
	err = walletdb.Update(w.Database(), func(dbtx walletdb.ReadWriteTx) error {
		return w.Manager.SetSyncedTo(dbtx.ReadWriteBucket(addrmgrNamespace),
			&waddrmgr.BlockStamp{
				Height:    maturity,
				Hash:      chainhash.Hash{byte(maturity)},
				Timestamp: time.Now(),
			})
	})
	if err != nil {
		t.Fatal(err)
	}

	s := &walletServer{}
	s.wallet.Store(w)
	ctx := context.Background()

	sweep, err := s.SweepAccount(ctx, &pb.SweepAccountRequest{
		SweepToAddress: addr.String(),
	})
	if err != nil {
		t.Fatalf("unable to sweep: %v", err)
	}
	var tx wire.MsgTx
	if err := tx.BchDecode(bytes.NewReader(sweep.SerializedTransaction),
		wire.ProtocolVersion, wire.BaseEncoding); err != nil {
		t.Fatal(err)
	}
	spent := make([]wire.OutPoint, 0, len(tx.TxIn))
	for _, in := range tx.TxIn {
		spent = append(spent, in.PreviousOutPoint)
	}
	if len(spent) != 1 || spent[0] != mature {
		t.Fatalf("sweep spends %v, want only %v", spent, mature)
	}

	for _, test := range []struct {
		include bool
		want    map[wire.OutPoint]bool
	}{
		{false, map[wire.OutPoint]bool{mature: true}},
		{true, map[wire.OutPoint]bool{mature: true, immature: true}},
	} {
		resp, err := s.FundTransaction(ctx, &pb.FundTransactionRequest{
			IncludeImmatureCoinbases: test.include,
		})
		if err != nil {
			t.Fatalf("unable to fund: %v", err)
		}
		got := make(map[wire.OutPoint]bool)
		for _, out := range resp.SelectedOutputs {
			var hash chainhash.Hash
			copy(hash[:], out.TransactionHash)
			got[wire.OutPoint{Hash: hash, Index: out.OutputIndex}] = true
		}
		if len(got) != len(test.want) {
			t.Fatalf("include_immature_coinbases %v: got %v, want %v",
				test.include, got, test.want)
		}
		for op := range test.want {
			if !got[op] {
				t.Fatalf("include_immature_coinbases %v: got %v, "+
					"want %v", test.include, got, test.want)
			}
		}
	}
}
