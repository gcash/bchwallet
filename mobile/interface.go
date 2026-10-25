package mobile

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/gcash/bchwallet/boot"
)

// stopTimeout bounds how long StopWallet, and StartWallet for a wallet that is
// still shutting down, wait for the shutdown to complete.
const stopTimeout = 30 * time.Second

// walletRun is one run of the wallet, from StartWallet until it has shut down.
type walletRun struct {
	// cancel shuts the run down.
	cancel context.CancelFunc

	// done is closed once the run has shut down.
	done chan struct{}

	// err is the error the run ended with.  It is set before done is
	// closed.
	err error

	// stopRequested is set once StopWallet was called for the run.  It is
	// guarded by mtx.
	stopRequested bool
}

var (
	mtx sync.Mutex

	// current is the last run started, or nil before the first.
	current *walletRun

	// startErr is why StartWallet last failed to start the wallet.
	startErr error
)

func init() {
	// The app that embeds the wallet keeps its own signal handling, and the
	// wallet is configured only by its config file, not by the command
	// line of the app's process.
	boot.HandleSignals = false
	boot.Args = []string{}
}

// StartWallet is the function exposed to the mobile device to start the bchwallet.
// configPath is the path to the bchwallet.conf file that should be saved on your mobile device.
//
// Make sure you save in the config file the correct path on the device to use for `appdata`.
// You will likely also want to the `noinitalload` option to prevent the wallet from blocking
// startup as it waits for input from stdin.
//
// Once the wallet is started you will want to control it using the gRPC API. A `CreateWallet` RPC
// is available which you will need to call first.
//
// StartWallet returns right away and the wallet keeps running in the background until StopWallet
// is called. It does nothing while the wallet is running. If the wallet is still shutting down,
// it first waits for the shutdown to complete. Use LastError to find out why a run ended.
func StartWallet(configPath string) {
	mtx.Lock()
	defer mtx.Unlock()

	for current != nil && !isClosed(current.done) {
		r := current
		if !r.stopRequested {
			// Already running.
			return
		}
		mtx.Unlock()
		ok := waitStopped(r.done)
		mtx.Lock()
		if !ok {
			startErr = errors.New("the previous wallet has not shut down")
			return
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := &walletRun{cancel: cancel, done: make(chan struct{})}
	current, startErr = r, nil
	go func() {
		r.err = boot.WalletMainContext(ctx, &configPath)
		close(r.done)
	}()
}

// StopWallet will stop the wallet and perform a clean shutdown. It returns once the wallet has
// shut down and closed its databases, or after 30 seconds if the shutdown takes longer. The
// wallet can then be started again with StartWallet.
func StopWallet() {
	mtx.Lock()
	r := current
	if r != nil {
		r.stopRequested = true
	}
	mtx.Unlock()

	if r == nil {
		return
	}
	r.cancel()
	waitStopped(r.done)
}

// IsRunning returns whether the wallet is running, including while it starts up or shuts down.
func IsRunning() bool {
	mtx.Lock()
	r := current
	mtx.Unlock()

	return r != nil && !isClosed(r.done)
}

// LastError returns why the wallet last failed to start or why its last run ended, such as an
// invalid config file or an RPC port that is in use, or an empty string if there was no error or
// the wallet is still running.
func LastError() string {
	mtx.Lock()
	r, err := current, startErr
	mtx.Unlock()

	if err == nil && r != nil && isClosed(r.done) {
		err = r.err
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// isClosed returns whether the channel is closed.
func isClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// waitStopped waits for done to be closed, for at most stopTimeout, and returns whether it was.
func waitStopped(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	case <-time.After(stopTimeout):
		return false
	}
}
