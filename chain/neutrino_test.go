package chain

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gcash/neutrino"
)

// TestRescanFailed checks that a rescan that stopped, as one does when a new
// rescan replaces it, isn't taken for one that failed, which the client logged
// as an error after every rescan from the wallet's birthday.
func TestRescanFailed(t *testing.T) {
	if rescanFailed(nil) {
		t.Error("a rescan that ended without an error failed")
	}
	if rescanFailed(neutrino.ErrRescanExit) {
		t.Error("a rescan that stopped failed")
	}
	if rescanFailed(fmt.Errorf("replaced: %w", neutrino.ErrRescanExit)) {
		t.Error("a rescan that stopped, with the error wrapped, failed")
	}
	if !rescanFailed(errors.New("no filter for block")) {
		t.Error("a rescan that ended with another error didn't fail")
	}
}

// TestRetryFetch checks that a fetch that fails, as one of a filter that no
// peer sends, is tried again until it succeeds rather than given up on after a
// few tries, and that the retries end when neutrino shuts down or the client
// stops.
func TestRetryFetch(t *testing.T) {
	calls := 0
	err := retryFetch("filter", func() error {
		calls++
		if calls < 4 {
			return errors.New("no peer sent the filter")
		}
		return nil
	}, nil, time.Millisecond, 2*time.Millisecond)
	if err != nil || calls != 4 {
		t.Fatalf("a fetch that failed three times gave %v after %d "+
			"calls, want success after 4", err, calls)
	}

	calls = 0
	err = retryFetch("filter", func() error {
		calls++
		return fmt.Errorf("query ended: %w", neutrino.ErrShuttingDown)
	}, nil, time.Millisecond, time.Millisecond)
	if !errors.Is(err, neutrino.ErrShuttingDown) || calls != 1 {
		t.Fatalf("a fetch that failed as neutrino shut down gave %v "+
			"after %d calls, want its error after 1", err, calls)
	}

	quit := make(chan struct{})
	close(quit)
	err = retryFetch("filter", func() error {
		return errors.New("no peer sent the filter")
	}, quit, time.Hour, time.Hour)
	if !errors.Is(err, errClientStopped) {
		t.Fatalf("a fetch retried after the client stopped gave %v, "+
			"want errClientStopped", err)
	}
}
