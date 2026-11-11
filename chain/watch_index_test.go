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
// index has the same watch list and finds the same addresses and spends as
// one with the address and outpoint maps, and that building its watch list
// leaves the index's scripts as they are.
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
	for op, a := range outPoints {
		if err := watch.AddOutPoint(op, a); err != nil {
			t.Fatal(err)
		}
	}
	// Adding one again changes nothing.
	if err := watch.Add(waddrmgr.ScopedIndex{Scope: waddrmgr.KeyScopeBIP0044}, addr(1), false); err != nil {
		t.Fatal(err)
	}
	if err := watch.AddOutPoint(wire.OutPoint{Hash: chainhash.Hash{9}, Index: 2}, addr(4)); err != nil {
		t.Fatal(err)
	}
	scripts := len(watch.Scripts)

	withMaps := &FilterBlocksRequest{ExternalAddrs: external, InternalAddrs: internal,
		WatchedOutPoints: outPoints}
	withWatch := &FilterBlocksRequest{Watch: watch}

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

	// A spend of the watched outpoint, and of another.
	for _, op := range []wire.OutPoint{{Hash: chainhash.Hash{9}, Index: 2}, {Hash: chainhash.Hash{9}}} {
		spend := wire.NewMsgTx(wire.TxVersion)
		spend.AddTxIn(wire.NewTxIn(&op, nil))
		fm := NewBlockFilterer(params, withMaps)
		fw := NewBlockFilterer(params, withWatch)
		if fm.FilterTx(spend) != fw.FilterTx(spend) {
			t.Errorf("the filterers found the spend of %v differently", op)
		}
	}
}

// TestWatchListWithIndexCostsNothingPerOutPoint checks that building the watch
// list of a request with a watch index doesn't build a script and an outpoint
// for each outpoint watched, which recovery did for each block it found the
// wallet's addresses in, while the outpoints only grow.
func TestWatchListWithIndexCostsNothingPerOutPoint(t *testing.T) {
	params := &chaincfg.SimNetParams
	allocs := func(n int) float64 {
		watch := NewWatchIndex()
		for i := 0; i < n; i++ {
			hash := make([]byte, 20)
			hash[0], hash[1] = byte(i), byte(i>>8)
			a, err := bchutil.NewAddressPubKeyHash(hash, params)
			if err != nil {
				t.Fatal(err)
			}
			op := wire.OutPoint{Hash: chainhash.Hash{byte(i), byte(i >> 8)}, Index: uint32(i)}
			if err := watch.AddOutPoint(op, a); err != nil {
				t.Fatal(err)
			}
		}
		if len(watch.Scripts) != 2*n {
			t.Fatalf("the watch has %d scripts for %d outpoints", len(watch.Scripts), n)
		}
		req := &FilterBlocksRequest{Watch: watch}
		return testing.AllocsPerRun(20, func() {
			if _, err := buildFilterBlocksWatchList(req); err != nil {
				t.Fatal(err)
			}
		})
	}
	few, many := allocs(10), allocs(2000)
	if many > few+5 {
		t.Fatalf("a watch list made %.0f allocations with 2000 outpoints, and %.0f with 10",
			many, few)
	}
}
