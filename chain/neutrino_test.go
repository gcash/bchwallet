package chain

import (
	"errors"
	"fmt"
	"testing"

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
