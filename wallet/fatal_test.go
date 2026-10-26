package wallet

import (
	"errors"
	"testing"
)

// TestFatalErrorHandler ensures a fatal error goes to the fatal error handler,
// and panics without one.
func TestFatalErrorHandler(t *testing.T) {
	boom := errors.New("boom")

	var w Wallet
	var got error
	w.SetFatalErrorHandler(func(err error) { got = err })
	w.fatal(boom)
	if !errors.Is(got, boom) {
		t.Fatalf("handler got %v, want %v", got, boom)
	}

	defer func() {
		if r := recover(); r != boom {
			t.Fatalf("without a handler: recovered %v, want a panic with %v",
				r, boom)
		}
	}()
	var unhandled Wallet
	unhandled.fatal(boom)
}
