package wallet

import (
	"strings"
	"testing"
	"time"

	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchutil/hdkeychain"
)

// TestLoaderHooks checks that OnLoad sets up every wallet the loader loads,
// also one loaded again after an unload and one already loaded when it is
// added, before the functions RunAfterLoad added, and that OnUnload runs before
// each unload.  A wallet opened again over gRPC then has the fatal error
// handler that the one-shot RunAfterLoad callback only gave the first.
func TestLoaderHooks(t *testing.T) {
	seed, err := hdkeychain.GenerateSeed(hdkeychain.MinSeedBytes)
	if err != nil {
		t.Fatal(err)
	}
	loader := NewLoader(&chaincfg.RegressionNetParams, t.TempDir(), true, 250)
	t.Cleanup(func() { _ = loader.UnloadWallet() })

	var events []string
	loader.OnLoad(func(w *Wallet) {
		events = append(events, "load hook")
		w.SetFatalErrorHandler(func(error) {})
	})
	loader.OnUnload(func(*Wallet) { events = append(events, "unload hook") })
	loader.RunAfterLoad(func(*Wallet) { events = append(events, "after load") })

	first, err := loader.CreateNewWallet([]byte("hello"), []byte("world"),
		seed, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := loader.UnloadWallet(); err != nil {
		t.Fatal(err)
	}
	second, err := loader.OpenExistingWallet([]byte("hello"), false)
	if err != nil {
		t.Fatal(err)
	}
	loader.OnLoad(func(*Wallet) { events = append(events, "late hook") })
	if err := loader.UnloadWallet(); err != nil {
		t.Fatal(err)
	}

	want := "load hook, after load, unload hook, load hook, late hook, " +
		"unload hook"
	if got := strings.Join(events, ", "); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	for i, w := range []*Wallet{first, second} {
		w.fatalMtx.Lock()
		handled := w.fatalErrorHandler != nil
		w.fatalMtx.Unlock()
		if !handled {
			t.Errorf("wallet %d has no fatal error handler", i)
		}
	}
}

// TestLoaderRestartWallet checks that RestartWallet restarts the loaded
// wallet, and leaves a wallet that was unloaded meanwhile stopped, rather than
// starting it again on its closed database, as the chain backend did when its
// client dropped just as the shutdown unloaded the wallet.
func TestLoaderRestartWallet(t *testing.T) {
	seed, err := hdkeychain.GenerateSeed(hdkeychain.MinSeedBytes)
	if err != nil {
		t.Fatal(err)
	}
	loader := NewLoader(&chaincfg.RegressionNetParams, t.TempDir(), true, 250)
	t.Cleanup(func() { _ = loader.UnloadWallet() })

	w, err := loader.CreateNewWallet([]byte("hello"), []byte("world"), seed,
		time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !loader.RestartWallet(w) || w.ShuttingDown() {
		t.Fatal("the loaded wallet wasn't restarted")
	}

	if err := loader.UnloadWallet(); err != nil {
		t.Fatal(err)
	}
	if loader.RestartWallet(w) {
		t.Fatal("restarted a wallet that was unloaded")
	}
	if !w.ShuttingDown() {
		t.Fatal("the unloaded wallet runs again")
	}
}
