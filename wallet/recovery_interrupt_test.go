package wallet

import (
	"errors"
	"testing"
	"time"

	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchwallet/chain"
	"github.com/gcash/bchwallet/wtxmgr"
)

// notifyingChainClient is a mock chain client whose notifications a test
// sends.
type notifyingChainClient struct {
	mockChainClient
	ntfns chan interface{}
}

func (c *notifyingChainClient) Notifications() <-chan interface{} {
	return c.ntfns
}

// newStoppingWallet returns a wallet with the chain client, whose recovery
// interrupt handler is not running, as when it has returned because the
// wallet stops.
func newStoppingWallet(client chain.Interface) *Wallet {
	return &Wallet{
		quit:                  make(chan struct{}),
		recoveryInterruptChan: make(chan struct{}),
		chainClient:           client,
	}
}

// waitFor fails the test if f does not return within a few seconds.
func waitFor(t *testing.T, what string, f func()) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		f()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return after the wallet stopped", what)
	}
}

// TestStopDuringRelevantTx ensures the chain notification handler returns
// when the wallet stops while it passes on a relevant transaction, which it
// did by interrupting the recovery through a handler that had already
// returned, so it blocked forever and so did the wallet's shutdown.
func TestStopDuringRelevantTx(t *testing.T) {
	client := &notifyingChainClient{ntfns: make(chan interface{})}
	w := newStoppingWallet(client)
	w.wg.Add(1)
	go w.handleChainNotifications()

	rec, err := wtxmgr.NewTxRecordFromMsgTx(wire.NewMsgTx(wire.TxVersion),
		time.Now())
	if err != nil {
		t.Fatal(err)
	}
	client.ntfns <- chain.RelevantTx{TxRecord: rec}

	w.Stop()
	waitFor(t, "the chain notification handler", w.wg.Wait)
}

// TestPublishDuringStop ensures publishing a transaction while the wallet
// stops fails with ErrWalletShuttingDown instead of blocking forever.
func TestPublishDuringStop(t *testing.T) {
	w := newStoppingWallet(&mockChainClient{})

	// Stop closes quit before it drops the chain client, so a publish can
	// still find the client then.
	close(w.quit)

	var err error
	waitFor(t, "PublishTransaction", func() {
		err = w.PublishTransaction(wire.NewMsgTx(wire.TxVersion))
	})
	if !errors.Is(err, ErrWalletShuttingDown) {
		t.Fatalf("got %v, want %v", err, ErrWalletShuttingDown)
	}
}
