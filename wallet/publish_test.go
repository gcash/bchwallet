package wallet

import (
	"errors"
	"testing"

	"github.com/gcash/bchd/btcjson"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/walletdb"
	"github.com/gcash/neutrino/pushtx"
)

// brokenChainClient is a chain backend whose broadcast of a transaction fails
// with the error errs has for its hash, if any.
type brokenChainClient struct {
	mockChainClient
	errs map[chainhash.Hash]error
}

func (c *brokenChainClient) SendRawTransaction(tx *wire.MsgTx, _ bool) (
	*chainhash.Hash, error) {

	hash := tx.TxHash()
	if err := c.errs[hash]; err != nil {
		return nil, err
	}
	return &hash, nil
}

// broadcastErrors are ways a broadcast fails, with whether PublishTransaction
// returns the error and whether the wallet keeps the transaction as unmined,
// to broadcast again.
var broadcastErrors = []struct {
	name    string
	err     error
	wantErr bool
	kept    bool
}{{
	name: "stopped SPV backend",
	err:  pushtx.ErrBroadcasterStopped,
	kept: true,
}, {
	name: "unreachable node",
	err:  errors.New("dial tcp 127.0.0.1:18334: connect: connection refused"),
	kept: true,
}, {
	name: "invalid to the SPV peers",
	err: &pushtx.BroadcastError{
		Code:   pushtx.Invalid,
		Reason: "rejected by 127.0.0.1:18444: bad-txns-in-belowout",
	},
	wantErr: true,
}, {
	name: "too cheap for the SPV peers",
	err: &pushtx.BroadcastError{
		Code:   pushtx.InsufficientFee,
		Reason: "rejected by 127.0.0.1:18444: min relay fee not met",
	},
	wantErr: true,
}, {
	name: "rejected by the node",
	err: &btcjson.RPCError{
		Code:    btcjson.ErrRPCTxRejected,
		Message: "bad-txns-in-belowout, inputs below outputs (code 16)",
	},
	wantErr: true,
}, {
	// These reasons don't say what happened, so the codes must.
	name: "in the SPV peers' mempools",
	err: &pushtx.BroadcastError{
		Code:   pushtx.Mempool,
		Reason: "rejected by 127.0.0.1:18444: duplicate",
	},
	kept: true,
}, {
	name: "in the SPV peers' chain",
	err: &pushtx.BroadcastError{
		Code:   pushtx.Confirmed,
		Reason: "rejected by 127.0.0.1:18444: duplicate",
	},
}}

// failingSpends returns a transaction for each of broadcastErrors, spending an
// output of w's, and a backend whose broadcast of each fails with its case's
// error.
func failingSpends(t *testing.T, w *Wallet) ([]*wire.MsgTx, *brokenChainClient) {
	t.Helper()

	script := walletScript(t, w)
	backend := &brokenChainClient{errs: make(map[chainhash.Hash]error)}
	txs := make([]*wire.MsgTx, len(broadcastErrors))
	for i, test := range broadcastErrors {
		op := addCredit(t, w, wire.NewTxOut(100000+int64(i), script,
			wire.TokenData{}))
		txs[i] = spendTx([]wire.OutPoint{op},
			wire.NewTxOut(90000, script, wire.TokenData{}))
		backend.errs[txs[i].TxHash()] = test.err
	}
	return txs, backend
}

// TestPublishTransactionErrors checks that a failed broadcast removes the
// transaction from the wallet only when the network rejected it, and that
// PublishTransaction reports a transaction it keeps, to broadcast again, as
// published.
func TestPublishTransactionErrors(t *testing.T) {
	w := newRegtestWallet(t)
	txs, backend := failingSpends(t, w)
	w.chainClient = backend

	for i, test := range broadcastErrors {
		err := w.PublishTransaction(txs[i])
		if (err != nil) != test.wantErr {
			t.Errorf("%s: got error %v, want one: %v", test.name, err,
				test.wantErr)
		}
		if kept := recorded(t, w, txs[i]); kept != test.kept {
			t.Errorf("%s: kept the transaction: %v, want %v",
				test.name, kept, test.kept)
		}
	}
}

// unwatchingChainClient is a chain backend that can't take requests to watch
// addresses, as one that is stopping.
type unwatchingChainClient struct {
	brokenChainClient
}

func (c *unwatchingChainClient) NotifyReceived([]bchutil.Address) error {
	return errors.New("the client has been shutdown")
}

// TestPublishTransactionNotifyError checks that a backend that can't watch the
// outputs of a transaction the wallet has recorded doesn't make the publish
// fail, since the wallet keeps the transaction and broadcasts it again, so a
// caller that created it again would pay twice.
func TestPublishTransactionNotifyError(t *testing.T) {
	w := newRegtestWallet(t)
	script := walletScript(t, w)
	op := addCredit(t, w, wire.NewTxOut(100000, script, wire.TokenData{}))
	tx := spendTx([]wire.OutPoint{op},
		wire.NewTxOut(90000, script, wire.TokenData{}))
	w.chainClient = &unwatchingChainClient{}

	if err := w.PublishTransaction(tx); err != nil {
		t.Fatalf("got %v for a transaction the wallet keeps", err)
	}
	if !recorded(t, w, tx) {
		t.Fatal("the wallet didn't keep the transaction")
	}
}

// TestResendUnminedTxsErrors checks that rebroadcasting the wallet's unmined
// transactions, as it does after each block, removes one only when the network
// rejected it.
func TestResendUnminedTxsErrors(t *testing.T) {
	w := newRegtestWallet(t)
	txs, backend := failingSpends(t, w)
	for _, tx := range txs {
		insertTx(t, w, tx, nil)
	}
	w.chainClient = backend

	w.resendUnminedTxs()
	for i, test := range broadcastErrors {
		if kept := recorded(t, w, txs[i]); kept != test.kept {
			t.Errorf("%s: kept the transaction: %v, want %v",
				test.name, kept, test.kept)
		}
	}
}

// TestNotifyUnminedDuringBlock checks that notifying an unmined transaction, as
// publishing one does, doesn't read the notification of mined transactions,
// which the chain goroutine builds while it handles a block.  The race detector
// catches the read.
func TestNotifyUnminedDuringBlock(t *testing.T) {
	w := newRegtestWallet(t)
	script := walletScript(t, w)
	op := addCredit(t, w, wire.NewTxOut(100000, script, wire.TokenData{}))
	rec := insertTx(t, w, spendTx([]wire.OutPoint{op},
		wire.NewTxOut(90000, script, wire.TokenData{})), nil, 0)

	s := w.NtfnServer
	err := walletdb.View(w.db, func(dbtx walletdb.ReadTx) error {
		ns := dbtx.ReadBucket(wtxmgrNamespaceKey)
		details, err := w.TxStore.TxDetails(ns, &rec.Hash)
		if err != nil {
			return err
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			s.currentTxNtfn = &TransactionNotifications{}
			s.currentTxNtfn = nil
		}()
		s.notifyUnminedTransaction(dbtx, details)
		<-done
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
