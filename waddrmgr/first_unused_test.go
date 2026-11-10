// Copyright (c) 2026 The bchwallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package waddrmgr

import (
	"testing"

	"github.com/gcash/bchwallet/walletdb"
)

// firstUnused returns the first unused address of the default account's
// branch.
func firstUnused(t *testing.T, db walletdb.DB, scoped *ScopedKeyManager,
	internal bool) ManagedAddress {

	t.Helper()
	var first ManagedAddress
	err := walletdb.View(db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		var err error
		first, err = scoped.FirstUnusedAddress(ns, DefaultAccountNum, internal)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return first
}

// markUsed marks the addresses used.
func markUsed(t testing.TB, db walletdb.DB, mgr *Manager, addrs ...ManagedAddress) {
	t.Helper()
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		for _, addr := range addrs {
			if err := mgr.MarkUsed(ns, addr.Address()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestFirstUnusedAddressIsTheLowest ensures that the first unused address is
// the one of its branch with the lowest index that wasn't used, whichever
// branch the others are on.
func TestFirstUnusedAddressIsTheLowest(t *testing.T) {
	teardown, db, mgr := setupManager(t)
	defer teardown()
	scoped, err := mgr.FetchScopedKeyManager(KeyScopeBIP0044)
	if err != nil {
		t.Fatal(err)
	}
	external, internal := deriveBoth(t, db, scoped, 30)
	markUsed(t, db, mgr, external[0], external[1], external[2], external[4],
		internal[0])

	if got := firstUnused(t, db, scoped, false); got.Address().String() !=
		external[3].Address().String() {

		t.Fatalf("the first unused external address is %v, want %v",
			got.Address(), external[3].Address())
	}
	if got := firstUnused(t, db, scoped, true); got.Address().String() !=
		internal[1].Address().String() {

		t.Fatalf("the first unused internal address is %v, want %v",
			got.Address(), internal[1].Address())
	}

	markUsed(t, db, mgr, external[3])
	if got := firstUnused(t, db, scoped, false); got.Address().String() !=
		external[5].Address().String() {

		t.Fatalf("the first unused external address is %v, want %v",
			got.Address(), external[5].Address())
	}
}

// BenchmarkFirstUnusedAddress finds the first unused address of an account
// with 1000 addresses, all but the last used.  Each was derived and its key
// parsed to tell if it was used, which made the address popup slow for a wallet
// with thousands of addresses.
func BenchmarkFirstUnusedAddress(b *testing.B) {
	teardown, db, mgr := setupManager(b)
	defer teardown()
	scoped, err := mgr.FetchScopedKeyManager(KeyScopeBIP0044)
	if err != nil {
		b.Fatal(err)
	}
	const n = 1000
	external, _ := deriveBoth(b, db, scoped, n)
	markUsed(b, db, mgr, external[:n-1]...)
	b.ResetTimer()
	err = walletdb.View(db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		for i := 0; i < b.N; i++ {
			if _, err := scoped.FirstUnusedAddress(ns, DefaultAccountNum, false); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
}
