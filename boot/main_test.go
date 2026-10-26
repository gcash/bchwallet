package boot

import (
	"errors"
	"testing"
	"time"
)

// TestStopWith ensures that a fatal wallet error stops the run, and that the
// run keeps the first error that stopped it.
func TestStopWith(t *testing.T) {
	handleSignals := HandleSignals
	HandleSignals = false
	defer func() { HandleSignals = handleSignals }()

	interrupts := newInterruptState()
	defer interrupts.shutdown()

	var stopped stopError
	if err := stopped.get(); err != nil {
		t.Fatalf("got %v before any error", err)
	}

	first := errors.New("unable to synchronize wallet to chain")
	stopped.stopWith(interrupts, first)
	stopped.stopWith(interrupts, errors.New("later error"))

	select {
	case <-interrupts.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run was not stopped")
	}
	if err := stopped.get(); !errors.Is(err, first) {
		t.Fatalf("got %v, want %v", err, first)
	}
}
