// Copyright (c) 2026 The bchwallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

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
	"github.com/gcash/bchwallet/wallet/txrules"
	"github.com/gcash/bchwallet/walletdb"
	"github.com/gcash/bchwallet/wtxmgr"
)

// sweepWallet returns a wallet holding a coin of each value, and the server
// for it.
func sweepWallet(t *testing.T, values []int64) (*walletServer, string) {
	t.Helper()
	seed, err := hdkeychain.GenerateSeed(hdkeychain.MinSeedBytes)
	if err != nil {
		t.Fatal(err)
	}
	params := &chaincfg.RegressionNetParams
	loader := wallet.NewLoader(params, t.TempDir(), true, 250)
	w, err := loader.CreateNewWallet([]byte("public"), []byte("private"), seed, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := loader.UnloadWallet(); err != nil {
			t.Error(err)
		}
	})
	addr, err := w.CurrentAddress(0, waddrmgr.KeyScopeBIP0044)
	if err != nil {
		t.Fatal(err)
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatal(err)
	}
	block := &wtxmgr.BlockMeta{
		Block: wtxmgr.Block{Hash: chainhash.Hash{1}, Height: 1},
		Time:  time.Unix(1700000000, 0),
	}
	err = walletdb.Update(w.Database(), func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadWriteBucket(txmgrNamespace)
		for i, value := range values {
			tx := wire.NewMsgTx(wire.TxVersion)
			tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{
				Hash:  chainhash.Hash{byte(i), byte(i >> 8), 0xee},
				Index: uint32(i),
			}, nil))
			tx.AddTxOut(wire.NewTxOut(value, script, wire.TokenData{}))
			rec, err := wtxmgr.NewTxRecordFromMsgTx(tx, time.Now())
			if err != nil {
				return err
			}
			if err := w.TxStore.InsertTx(ns, rec, block); err != nil {
				return err
			}
			if err := w.TxStore.AddCredit(ns, rec, block, 0, false); err != nil {
				return err
			}
		}
		return w.Manager.SetSyncedTo(dbtx.ReadWriteBucket(addrmgrNamespace),
			&waddrmgr.BlockStamp{Height: 10, Hash: chainhash.Hash{10}, Timestamp: time.Now()})
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &walletServer{}
	s.wallet.Store(w)
	return s, addr.String()
}

// TestSweepFitsOneTransaction checks that a sweep of more coins than fit in
// one standard transaction takes the largest that fit, and says how many it
// left and their value, as a sweep of them all was refused by the network.
func TestSweepFitsOneTransaction(t *testing.T) {
	values := make([]int64, 1000)
	var total int64
	for i := range values {
		// In no order, so that the sweep has to pick the largest.
		values[i] = int64(1000 + (i*7919)%1000*10)
		total += values[i]
	}
	s, to := sweepWallet(t, values)
	sweep, err := s.SweepAccount(context.Background(), &pb.SweepAccountRequest{SweepToAddress: to})
	if err != nil {
		t.Fatal(err)
	}
	var tx wire.MsgTx
	if err := tx.BchDecode(bytes.NewReader(sweep.SerializedTransaction), wire.ProtocolVersion,
		wire.BaseEncoding); err != nil {
		t.Fatal(err)
	}
	n := len(tx.TxIn)
	if n == 0 || n == len(values) {
		t.Fatalf("the sweep took %d of %d coins", n, len(values))
	}
	// Signed, each input grows by its signature script.
	signed := tx.SerializeSize() + n*107
	if signed > txrules.MaxStandardTxSize {
		t.Fatalf("the signed sweep is %d bytes, more than %d", signed, txrules.MaxStandardTxSize)
	}
	var taken, smallestTaken int64
	smallestTaken = 1 << 62
	for _, v := range sweep.InputValues {
		taken += v
		if v < smallestTaken {
			smallestTaken = v
		}
	}
	if sweep.LeftCount != uint32(len(values)-n) || sweep.LeftAmount != total-taken {
		t.Fatalf("left %d coins of %d sat, want %d of %d", sweep.LeftCount, sweep.LeftAmount,
			len(values)-n, total-taken)
	}
	// The largest first: none left out is larger than one taken.
	for _, v := range values {
		left := true
		for _, in := range sweep.InputValues {
			if in == v {
				left = false
				break
			}
		}
		if left && v > smallestTaken {
			t.Fatalf("a coin of %d was left out, and one of %d taken", v, smallestTaken)
		}
	}
}

// TestSweepOfFewCoinsLeavesNone checks that a sweep that fits takes every coin.
func TestSweepOfFewCoinsLeavesNone(t *testing.T) {
	s, to := sweepWallet(t, []int64{5000, 7000, 9000})
	sweep, err := s.SweepAccount(context.Background(), &pb.SweepAccountRequest{SweepToAddress: to})
	if err != nil {
		t.Fatal(err)
	}
	if len(sweep.InputValues) != 3 || sweep.LeftCount != 0 || sweep.LeftAmount != 0 {
		t.Fatalf("swept %d coins and left %d of %d sat, want 3 and none",
			len(sweep.InputValues), sweep.LeftCount, sweep.LeftAmount)
	}
}
