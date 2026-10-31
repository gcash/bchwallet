// Copyright (c) 2018 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"bytes"
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil/hdkeychain"
	"github.com/gcash/bchwallet/waddrmgr"
	"github.com/gcash/bchwallet/wallet/txrules"
	"github.com/gcash/bchwallet/walletdb"
	_ "github.com/gcash/bchwallet/walletdb/bdb"
	"github.com/gcash/bchwallet/wtxmgr"
)

// TestTxToOutput checks that no new address is added to he database if we
// request a dry run of the txToOutputs call. It also makes sure a subsequent
// non-dry run call produces a similar transaction to the dry-run.
func TestTxToOutputsDryRun(t *testing.T) {
	// Set up a wallet.
	dir, err := os.MkdirTemp("", "createtx_test")
	if err != nil {
		t.Fatalf("Failed to create db dir: %v", err)
	}
	defer os.RemoveAll(dir)

	seed, err := hdkeychain.GenerateSeed(hdkeychain.MinSeedBytes)
	if err != nil {
		t.Fatalf("unable to create seed: %v", err)
	}

	pubPass := []byte("hello")
	privPass := []byte("world")

	loader := NewLoader(&chaincfg.TestNet3Params, dir, true, 250)
	w, err := loader.CreateNewWallet(pubPass, privPass, seed, time.Now())
	if err != nil {
		t.Fatalf("unable to create wallet: %v", err)
	}
	chainClient := &mockChainClient{}
	w.chainClient = chainClient
	if err := w.Unlock(privPass, time.After(10*time.Minute)); err != nil {
		t.Fatalf("unable to unlock wallet: %v", err)
	}

	// Create an address we can use to send some coins to.
	addr, err := w.CurrentAddress(0, waddrmgr.KeyScopeBIP0044)
	if err != nil {
		t.Fatalf("unable to get current address: %v", addr)
	}
	p2shAddr, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatalf("unable to convert wallet address to p2sh: %v", err)
	}

	// Add an output paying to the wallet's address to the database.
	txOut := wire.NewTxOut(100000, p2shAddr, wire.TokenData{})
	incomingTx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{},
		},
		TxOut: []*wire.TxOut{
			txOut,
		},
	}

	var b bytes.Buffer
	if err := incomingTx.Serialize(&b); err != nil {
		t.Fatalf("unable to serialize tx: %v", err)
	}
	txBytes := b.Bytes()

	rec, err := wtxmgr.NewTxRecord(txBytes, time.Now())
	if err != nil {
		t.Fatalf("unable to create tx record: %v", err)
	}

	// The block meta will be inserted to tell the wallet this is a
	// confirmed transaction.
	blockHash, _ := chainhash.NewHashFromStr(
		"00000000000000017188b968a371bab95aa43522665353b646e41865abae02a4")
	block := &wtxmgr.BlockMeta{
		Block: wtxmgr.Block{Hash: *blockHash, Height: 276425},
		Time:  time.Unix(1387737310, 0),
	}

	if err := walletdb.Update(w.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(wtxmgrNamespaceKey)
		err = w.TxStore.InsertTx(ns, rec, block)
		if err != nil {
			return err
		}
		return w.TxStore.AddCredit(ns, rec, block, 0, false)
	}); err != nil {
		t.Fatalf("failed inserting tx: %v", err)
	}

	// Now tell the wallet to create a transaction paying to the specified
	// outputs.
	txOuts := []*wire.TxOut{
		{
			PkScript: p2shAddr,
			Value:    10000,
		},
		{
			PkScript: p2shAddr,
			Value:    20000,
		},
	}

	// First do a few dry-runs, making sure the number of addresses in the
	// database us not inflated.
	dryRunTx, err := w.txToOutputs(txOuts, 0, 1, 1000, true)
	if err != nil {
		t.Fatalf("unable to author tx: %v", err)
	}
	change := dryRunTx.Tx.TxOut[dryRunTx.ChangeIndex]

	addresses, err := w.AccountAddresses(0)
	if err != nil {
		t.Fatalf("unable to get addresses: %v", err)
	}

	if len(addresses) != 20 {
		t.Fatalf("expected 20 addresses, found %v", len(addresses))
	}

	dryRunTx2, err := w.txToOutputs(txOuts, 0, 1, 1000, true)
	if err != nil {
		t.Fatalf("unable to author tx: %v", err)
	}
	change2 := dryRunTx2.Tx.TxOut[dryRunTx2.ChangeIndex]

	addresses, err = w.AccountAddresses(0)
	if err != nil {
		t.Fatalf("unable to get addresses: %v", err)
	}

	if len(addresses) != 20 {
		t.Fatalf("expected 20 addresses, found %v", len(addresses))
	}

	// The two dry-run TXs should be invalid, since they don't have
	// signatures.
	err = validateMsgTx(
		dryRunTx.Tx, dryRunTx.PrevScripts, dryRunTx.PrevInputValues,
		dryRunTx.PrevTokens, txscript.StandardVerifyFlags,
	)
	if err == nil {
		t.Fatalf("Expected tx to be invalid")
	}

	err = validateMsgTx(
		dryRunTx2.Tx, dryRunTx2.PrevScripts, dryRunTx2.PrevInputValues,
		dryRunTx2.PrevTokens, txscript.StandardVerifyFlags,
	)
	if err == nil {
		t.Fatalf("Expected tx to be invalid")
	}

	// Now we do a proper, non-dry run. This should add a change address
	// to the database.
	tx, err := w.txToOutputs(txOuts, 0, 1, 1000, false)
	if err != nil {
		t.Fatalf("unable to author tx: %v", err)
	}
	change3 := tx.Tx.TxOut[tx.ChangeIndex]

	addresses, err = w.AccountAddresses(0)
	if err != nil {
		t.Fatalf("unable to get addresses: %v", err)
	}

	if len(addresses) != 21 {
		t.Fatalf("expected 21 addresses, found %v", len(addresses))
	}

	err = validateMsgTx(tx.Tx, tx.PrevScripts, tx.PrevInputValues,
		tx.PrevTokens, txscript.StandardVerifyFlags)
	if err != nil {
		t.Fatalf("Expected tx to be valid: %v", err)
	}

	// Finally, we check that all the transaction were using the same
	// change address.
	if !bytes.Equal(change.PkScript, change2.PkScript) {
		t.Fatalf("first dry-run using different change address " +
			"than second")
	}
	if !bytes.Equal(change2.PkScript, change3.PkScript) {
		t.Fatalf("dry-run using different change address " +
			"than wet run")
	}
}

// txCountingDB is a walletdb.DB that records a goroutine opening a database
// transaction while it has another open.  bbolt can deadlock on that when a
// write in between has to remap the database file: the write waits for the
// open transaction to end, and the new transaction waits for the write.
type txCountingDB struct {
	walletdb.DB

	mtx    sync.Mutex
	open   map[uint64]int
	nested bool
}

func newTxCountingDB(db walletdb.DB) *txCountingDB {
	return &txCountingDB{DB: db, open: make(map[uint64]int)}
}

// opened records a transaction that the calling goroutine opened, and returns
// a function that records it closed.
func (db *txCountingDB) opened() func() {
	id := goroutineID()

	db.mtx.Lock()
	if db.open[id] > 0 {
		db.nested = true
	}
	db.open[id]++
	db.mtx.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			db.mtx.Lock()
			db.open[id]--
			db.mtx.Unlock()
		})
	}
}

// nestedTx returns whether a goroutine opened a transaction while it had
// another open.
func (db *txCountingDB) nestedTx() bool {
	db.mtx.Lock()
	defer db.mtx.Unlock()

	return db.nested
}

func (db *txCountingDB) BeginReadTx() (walletdb.ReadTx, error) {
	tx, err := db.DB.BeginReadTx()
	if err != nil {
		return nil, err
	}
	return &countedReadTx{ReadTx: tx, closed: db.opened()}, nil
}

func (db *txCountingDB) BeginReadWriteTx() (walletdb.ReadWriteTx, error) {
	tx, err := db.DB.BeginReadWriteTx()
	if err != nil {
		return nil, err
	}
	return &countedReadWriteTx{ReadWriteTx: tx, closed: db.opened()}, nil
}

type countedReadTx struct {
	walletdb.ReadTx
	closed func()
}

func (tx *countedReadTx) Rollback() error {
	defer tx.closed()
	return tx.ReadTx.Rollback()
}

type countedReadWriteTx struct {
	walletdb.ReadWriteTx
	closed func()
}

func (tx *countedReadWriteTx) Rollback() error {
	defer tx.closed()
	return tx.ReadWriteTx.Rollback()
}

func (tx *countedReadWriteTx) Commit() error {
	defer tx.closed()
	return tx.ReadWriteTx.Commit()
}

// goroutineID returns the ID of the calling goroutine, read from its stack
// trace, which starts "goroutine <ID> [".
func goroutineID() uint64 {
	var buf [64]byte
	stack := bytes.TrimPrefix(buf[:runtime.Stack(buf[:], false)],
		[]byte("goroutine "))
	id, _ := strconv.ParseUint(
		string(stack[:bytes.IndexByte(stack, ' ')]), 10, 64,
	)
	return id
}

// TestCreateUnsignedOneTransactionAtATime checks that CreateUnsignedTx doesn't
// open a database transaction inside another.  It read the change address in
// a read transaction of its own inside the one it held, which bbolt can
// deadlock on when a block written meanwhile grows the database file, hanging
// the send and every later write.
func TestCreateUnsignedOneTransactionAtATime(t *testing.T) {
	w := newRegtestWallet(t)
	script := walletScript(t, w)
	fungible := func(amount uint64) wire.TokenData {
		return wire.TokenData{
			CategoryID: testToken.CategoryID,
			Amount:     amount,
			BitField:   wire.HAS_AMOUNT,
		}
	}
	addCredit(t, w, wire.NewTxOut(100000, script, wire.TokenData{}))
	addCredit(t, w, wire.NewTxOut(1000, script, fungible(100)))

	db := newTxCountingDB(w.db)
	w.db = db

	// A token send asks for a change address for the token change as well
	// as for the BCH change.
	for _, test := range []struct {
		name string
		out  *wire.TxOut
	}{
		{"BCH", wire.NewTxOut(10000, script, wire.TokenData{})},
		{"tokens", wire.NewTxOut(0, script, fungible(40))},
	} {
		_, err := w.CreateUnsignedTx(0, []*wire.TxOut{test.out}, nil, 1,
			txrules.DefaultRelayFeePerKb)
		if err != nil {
			t.Fatalf("%s: unable to create transaction: %v", test.name,
				err)
		}
		if db.nestedTx() {
			t.Fatalf("%s: opened a database transaction inside "+
				"another", test.name)
		}
	}
}
