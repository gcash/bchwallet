// Copyright (c) 2026 The bchwallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package chain

import (
	"bytes"
	"sort"
	"testing"

	"github.com/gcash/bchd/bchec"
	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchutil/gcs/builder"
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
	a4 := addr(4)
	script4, err := txscript.PayToAddrScript(a4)
	if err != nil {
		t.Fatal(err)
	}
	outPoints := map[wire.OutPoint]WatchedOutput{
		{Hash: chainhash.Hash{9}, Index: 2}: {Addr: a4, PkScript: script4},
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
	for op, out := range outPoints {
		if err := watch.AddOutPoint(op, out.Addr, out.PkScript); err != nil {
			t.Fatal(err)
		}
	}
	// Adding one again changes nothing.
	if err := watch.Add(waddrmgr.ScopedIndex{Scope: waddrmgr.KeyScopeBIP0044}, addr(1), false); err != nil {
		t.Fatal(err)
	}
	if err := watch.AddOutPoint(wire.OutPoint{Hash: chainhash.Hash{9}, Index: 2}, a4, script4); err != nil {
		t.Fatal(err)
	}
	scripts := len(watch.Scripts)

	withMaps := &FilterBlocksRequest{ExternalAddrs: external, InternalAddrs: internal,
		WatchedOutPoints: outPoints}
	withWatch := &FilterBlocksRequest{Watch: watch}

	sortedFor := func(req *FilterBlocksRequest, scriptFilters bool) []string {
		list, err := buildFilterBlocksWatchList(req, scriptFilters)
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
	sorted := func(req *FilterBlocksRequest) []string {
		return sortedFor(req, false)
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

	// For script filters both hold the key of the script of each address
	// and of the script the outpoint pays, and no outpoint.
	var wantKeys []string
	for _, n := range []byte{1, 2, 3, 4} {
		script, err := txscript.PayToAddrScript(addr(n))
		if err != nil {
			t.Fatal(err)
		}
		key := builder.ScriptKey(script)
		wantKeys = append(wantKeys, string(key[:]))
	}
	sort.Strings(wantKeys)
	for name, req := range map[string]*FilterBlocksRequest{"maps": withMaps, "watch index": withWatch} {
		keys := sortedFor(req, true)
		if len(keys) != len(wantKeys) {
			t.Fatalf("%s: %d script keys, want %d", name, len(keys), len(wantKeys))
		}
		for i := range keys {
			if keys[i] != wantKeys[i] {
				t.Fatalf("%s: script key %d is %x, want %x", name, i, keys[i], wantKeys[i])
			}
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
			script, err := txscript.PayToAddrScript(a)
			if err != nil {
				t.Fatal(err)
			}
			if err := watch.AddOutPoint(op, a, script); err != nil {
				t.Fatal(err)
			}
		}
		if len(watch.Scripts) != 2*n {
			t.Fatalf("the watch has %d scripts for %d outpoints", len(watch.Scripts), n)
		}
		req := &FilterBlocksRequest{Watch: watch}
		return testing.AllocsPerRun(20, func() {
			if _, err := buildFilterBlocksWatchList(req, false); err != nil {
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

// TestScriptFilterWatchListUsesBareMultisigScript checks that a script filter
// watch list built from a bare multisig outpoint holds the key of that
// output's script, not the key of the pay-to-pubkey script of its first key,
// and that a regular filter watch list still holds the outpoint.
func TestScriptFilterWatchListUsesBareMultisigScript(t *testing.T) {
	params := &chaincfg.SimNetParams
	var pubs []*bchutil.AddressPubKey
	for range 2 {
		key, err := bchec.NewPrivateKey(bchec.S256())
		if err != nil {
			t.Fatal(err)
		}
		pub, err := bchutil.NewAddressPubKey(
			key.PubKey().SerializeCompressed(), params,
		)
		if err != nil {
			t.Fatal(err)
		}
		pubs = append(pubs, pub)
	}
	multisig, err := txscript.MultiSigScript(pubs, 1)
	if err != nil {
		t.Fatal(err)
	}
	op := wire.OutPoint{Hash: chainhash.Hash{7}, Index: 1}
	watched := map[wire.OutPoint]WatchedOutput{
		op: {Addr: pubs[0], PkScript: multisig},
	}

	wantKey := builder.ScriptKey(multisig)
	p2pk, err := txscript.PayToAddrScript(pubs[0])
	if err != nil {
		t.Fatal(err)
	}
	wrongKey := builder.ScriptKey(p2pk)
	var buf bytes.Buffer
	if err := op.Serialize(&buf); err != nil {
		t.Fatal(err)
	}

	watch := NewWatchIndex()
	if err := watch.AddOutPoint(op, pubs[0], multisig); err != nil {
		t.Fatal(err)
	}
	check := func(name string, req *FilterBlocksRequest) {
		t.Helper()
		keys, err := buildFilterBlocksWatchList(req, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(keys) != 1 || !bytes.Equal(keys[0], wantKey[:]) {
			t.Fatalf("%s: script filter watch list is %x, want the key of "+
				"the multisig script %x", name, keys, wantKey[:])
		}
		if bytes.Equal(keys[0], wrongKey[:]) {
			t.Fatalf("%s: script filter watch list holds the key of the "+
				"pay-to-pubkey script of the first key", name)
		}

		regular, err := buildFilterBlocksWatchList(req, false)
		if err != nil {
			t.Fatal(err)
		}
		var sawOutPoint, sawScript bool
		for _, item := range regular {
			if bytes.Equal(item, buf.Bytes()) {
				sawOutPoint = true
			}
			if bytes.Equal(item, multisig) {
				sawScript = true
			}
		}
		if !sawOutPoint {
			t.Fatalf("%s: regular filter watch list %x doesn't hold the "+
				"outpoint %x", name, regular, buf.Bytes())
		}
		if !sawScript {
			t.Fatalf("%s: regular filter watch list doesn't hold the "+
				"multisig script", name)
		}
	}
	check("maps", &FilterBlocksRequest{WatchedOutPoints: watched})
	check("watch index", &FilterBlocksRequest{Watch: watch})
}
