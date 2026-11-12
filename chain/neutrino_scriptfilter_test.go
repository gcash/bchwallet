// Copyright (c) 2026 The bchwallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package chain

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/integration/rpctest"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/waddrmgr"
	"github.com/gcash/bchwallet/walletdb"
	_ "github.com/gcash/bchwallet/walletdb/bdb"
	"github.com/gcash/bchwallet/wtxmgr"
	"github.com/gcash/neutrino"
)

// TestFilterBlocksScriptFilters checks end to end, against a node, that a
// neutrino client whose chain service syncs script filters (filter type 1)
// finds the block that pays an address it scans for, and then, watching the
// coin that payment made, the block that spends it.  A script filter doesn't
// list the outpoints a block spends, so the spend is found by the script the
// coin pays.
func TestFilterBlocksScriptFilters(t *testing.T) {
	node, err := rpctest.New(&chaincfg.SimNetParams, nil, []string{"--txindex"})
	if err != nil {
		t.Fatalf("unable to create harness: %v", err)
	}
	defer func() { _ = node.TearDown() }()
	if err := node.SetUp(true, 10); err != nil {
		t.Fatalf("unable to set up harness: %v", err)
	}

	// An address anyone can spend from, by showing its script.
	addr, err := bchutil.NewAddressScriptHash(
		[]byte{txscript.OP_TRUE}, &chaincfg.SimNetParams,
	)
	if err != nil {
		t.Fatal(err)
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatal(err)
	}
	payment, err := node.CreateTransaction(
		[]*wire.TxOut{{Value: 100000000, PkScript: script}}, 10, true,
	)
	if err != nil {
		t.Fatalf("unable to create payment: %v", err)
	}
	if _, err := node.Node.SendRawTransaction(payment, true); err != nil {
		t.Fatalf("unable to send payment: %v", err)
	}
	if _, err := node.Node.Generate(1); err != nil {
		t.Fatalf("unable to mine: %v", err)
	}
	_, paymentHeight, err := node.Node.GetBestBlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.Node.Generate(2); err != nil {
		t.Fatalf("unable to mine: %v", err)
	}

	paymentHash := payment.TxHash()
	var paid uint32
	for i, txOut := range payment.TxOut {
		if bytes.Equal(txOut.PkScript, script) {
			paid = uint32(i)
		}
	}
	outPoint := wire.NewOutPoint(&paymentHash, paid)
	spend := wire.NewMsgTx(2)
	spend.AddTxIn(wire.NewTxIn(outPoint, []byte{txscript.OP_DATA_1, txscript.OP_TRUE}))
	spend.AddTxOut(wire.NewTxOut(99990000, []byte{
		txscript.OP_DUP, txscript.OP_HASH160, txscript.OP_DATA_20,
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20,
		txscript.OP_EQUALVERIFY, txscript.OP_CHECKSIG,
	}, wire.TokenData{}))
	if _, err := node.Node.SendRawTransaction(spend, true); err != nil {
		t.Fatalf("unable to send spend: %v", err)
	}
	if _, err := node.Node.Generate(1); err != nil {
		t.Fatalf("unable to mine: %v", err)
	}
	_, spendHeight, err := node.Node.GetBestBlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.Node.Generate(2); err != nil {
		t.Fatalf("unable to mine: %v", err)
	}
	bestHash, tip, err := node.Node.GetBestBlock()
	if err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	db, err := walletdb.Create("bdb", filepath.Join(dataDir, "neutrino.db"), true)
	if err != nil {
		t.Fatalf("unable to create db: %v", err)
	}
	defer func() { _ = db.Close() }()
	svc, err := neutrino.NewChainService(neutrino.Config{
		DataDir:      dataDir,
		Database:     db,
		ChainParams:  chaincfg.SimNetParams,
		ConnectPeers: []string{node.P2PAddress()},
		FilterType:   wire.GCSFilterScripts,
	})
	if err != nil {
		t.Fatalf("unable to create ChainService: %v", err)
	}
	client := NewNeutrinoClient(&chaincfg.SimNetParams, svc)
	if err := client.Start(); err != nil {
		t.Fatalf("unable to start the client: %v", err)
	}
	defer func() {
		client.Stop()
		_ = svc.Stop()
	}()

	// Wait for the script filter headers of the whole chain.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, err := svc.FilterHeaders().FetchHeader(bestHash); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the script filter headers")
		}
		time.Sleep(100 * time.Millisecond)
	}

	blocks := func(from, to int32) []wtxmgr.BlockMeta {
		var metas []wtxmgr.BlockMeta
		for height := from; height <= to; height++ {
			hash, err := node.Node.GetBlockHash(int64(height))
			if err != nil {
				t.Fatal(err)
			}
			metas = append(metas, wtxmgr.BlockMeta{
				Block: wtxmgr.Block{Hash: *hash, Height: height},
			})
		}
		return metas
	}

	// Scanning for the address finds the block with the payment, and the
	// coin it made.
	index := waddrmgr.ScopedIndex{Scope: waddrmgr.KeyScopeBIP0044, Index: 0}
	resp, err := client.FilterBlocks(&FilterBlocksRequest{
		Blocks:        blocks(1, tip),
		ExternalAddrs: map[waddrmgr.ScopedIndex]bchutil.Address{index: addr},
	})
	if err != nil {
		t.Fatalf("unable to filter blocks: %v", err)
	}
	if resp == nil || resp.BlockMeta.Height != paymentHeight ||
		len(resp.RelevantTxns) != 1 || resp.RelevantTxns[0].TxHash() != paymentHash {

		t.Fatalf("scanning for the address found %+v, want the payment at "+
			"height %d", resp, paymentHeight)
	}
	if _, ok := resp.FoundOutPoints[*outPoint]; !ok {
		t.Fatalf("the payment's coin wasn't found: %v", resp.FoundOutPoints)
	}

	// Scanning on for the spend of that coin finds its block, by the
	// script the coin pays.
	resp, err = client.FilterBlocks(&FilterBlocksRequest{
		Blocks:           blocks(paymentHeight+1, tip),
		WatchedOutPoints: map[wire.OutPoint]bchutil.Address{*outPoint: addr},
	})
	if err != nil {
		t.Fatalf("unable to filter blocks: %v", err)
	}
	if resp == nil || resp.BlockMeta.Height != spendHeight ||
		len(resp.RelevantTxns) != 1 || resp.RelevantTxns[0].TxHash() != spend.TxHash() {

		t.Fatalf("scanning for the spend found %+v, want it at height %d",
			resp, spendHeight)
	}

	// The same with a prebuilt watch index, as a recovery scans.
	watch := NewWatchIndex()
	if err := watch.AddOutPoint(*outPoint, addr); err != nil {
		t.Fatal(err)
	}
	resp, err = client.FilterBlocks(&FilterBlocksRequest{
		Blocks:           blocks(paymentHeight+1, tip),
		Watch:            watch,
		WatchedOutPoints: watch.OutPoints,
	})
	if err != nil || resp == nil || resp.BlockMeta.Height != spendHeight {
		t.Fatalf("scanning with a watch index found %+v, %v", resp, err)
	}

	// Nothing is found past the spend.
	resp, err = client.FilterBlocks(&FilterBlocksRequest{
		Blocks:           blocks(spendHeight+1, tip),
		WatchedOutPoints: map[wire.OutPoint]bchutil.Address{*outPoint: addr},
	})
	if err != nil || resp != nil {
		t.Fatalf("scanning past the spend found %+v, %v", resp, err)
	}
}
