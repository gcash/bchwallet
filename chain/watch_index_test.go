// Copyright (c) 2026 The bchwallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package chain

import (
	"bytes"
	"sort"
	"testing"

	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/waddrmgr"
)

// TestWatchIndexMatchesTheMaps checks that a request with a prebuilt watch
// index has the same watch list and finds the same addresses as one with the
// address maps, and that building its watch list leaves the index's scripts
// as they are.
func TestWatchIndexMatchesTheMaps(t *testing.T) {
	params := &chaincfg.SimNetParams
	addr := func(b byte) bchutil.Address {
		a, err := bchutil.NewAddressPubKeyHash(bytes.Repeat([]byte{b}, 20), params)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	external := map[waddrmgr.ScopedIndex]bchutil.Address{
		{Scope: waddrmgr.KeyScopeBIP0044, Index: 0}: addr(1),
		{Scope: waddrmgr.KeyScopeBIP0044, Index: 1}: addr(2),
	}
	internal := map[waddrmgr.ScopedIndex]bchutil.Address{
		{Scope: waddrmgr.KeyScopeBIP0044, Index: 0}: addr(3),
	}
	outPoints := map[wire.OutPoint]bchutil.Address{
		{Hash: chainhash.Hash{9}, Index: 2}: addr(4),
	}
	watch := NewWatchIndex()
	for i, a := range external {
		if err := watch.Add(i, a, false); err != nil {
			t.Fatal(err)
		}
	}
	for i, a := range internal {
		if err := watch.Add(i, a, true); err != nil {
			t.Fatal(err)
		}
	}
	// Adding one again changes nothing.
	if err := watch.Add(waddrmgr.ScopedIndex{Scope: waddrmgr.KeyScopeBIP0044}, addr(1), false); err != nil {
		t.Fatal(err)
	}
	scripts := len(watch.Scripts)

	withMaps := &FilterBlocksRequest{ExternalAddrs: external, InternalAddrs: internal,
		WatchedOutPoints: outPoints}
	withWatch := &FilterBlocksRequest{Watch: watch, WatchedOutPoints: outPoints}

	sorted := func(req *FilterBlocksRequest) []string {
		list, err := buildFilterBlocksWatchList(req)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range list {
			out = append(out, string(s))
		}
		sort.Strings(out)
		return out
	}
	a, b := sorted(withMaps), sorted(withWatch)
	if len(a) != len(b) {
		t.Fatalf("watch lists of %d and %d entries", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("the watch lists differ at %d", i)
		}
	}
	if len(watch.Scripts) != scripts {
		t.Fatalf("building the watch list changed the index's %d scripts to %d",
			scripts, len(watch.Scripts))
	}

	payTo := func(a bchutil.Address) *wire.MsgTx {
		script, err := txscript.PayToAddrScript(a)
		if err != nil {
			t.Fatal(err)
		}
		tx := wire.NewMsgTx(wire.TxVersion)
		tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{1}}, nil))
		tx.AddTxOut(wire.NewTxOut(1000, script, wire.TokenData{}))
		return tx
	}
	for _, a := range []bchutil.Address{addr(2), addr(3), addr(5)} {
		fm := NewBlockFilterer(params, withMaps)
		fw := NewBlockFilterer(params, withWatch)
		if fm.FilterTx(payTo(a)) != fw.FilterTx(payTo(a)) ||
			len(fm.FoundExternal) != len(fw.FoundExternal) ||
			len(fm.FoundInternal) != len(fw.FoundInternal) {

			t.Errorf("the filterers found %v differently", a)
		}
	}
}
