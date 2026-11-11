// Copyright (c) 2026 The bchwallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"encoding/binary"
	"testing"

	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/waddrmgr"
)

// TestRecoveryWatchGrowsByWhatWasAdded checks that the recovery state's watch
// index takes the addresses its branches added since it last did, once each,
// so that building a filter blocks request costs what was added for it, not
// all the addresses watched, which recovery did for each block it found the
// wallet's addresses in.
func TestRecoveryWatchGrowsByWhatWasAdded(t *testing.T) {
	state := NewRecoveryState(10)
	scope := state.StateForScope(waddrmgr.KeyScopeBIP0044)
	addr := func(i uint32) bchutil.Address {
		hash := make([]byte, 20)
		binary.BigEndian.PutUint32(hash, i+1)
		a, err := bchutil.NewAddressPubKeyHash(hash, &chaincfg.SimNetParams)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	allocs := func(n uint32) float64 {
		for i := uint32(0); i < n; i++ {
			scope.ExternalBranch.AddAddr(i, addr(i))
			scope.InternalBranch.AddAddr(i, addr(i+1_000_000))
		}
		watch, err := state.Watch()
		if err != nil {
			t.Fatal(err)
		}
		if len(watch.Scripts) != int(2*n) {
			t.Fatalf("the watch has %d scripts for %d addresses", len(watch.Scripts), 2*n)
		}
		return testing.AllocsPerRun(20, func() {
			if _, err := newFilterBlocksRequest(nil, state); err != nil {
				t.Fatal(err)
			}
		})
	}
	few := allocs(50)
	many := allocs(2000)
	if many > few+5 {
		t.Fatalf("a request made %.0f allocations with 2000 addresses each way, "+
			"and %.0f with 50", many, few)
	}
}
