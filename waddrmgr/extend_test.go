// Copyright (c) 2026 The bchwallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package waddrmgr

import (
	"testing"

	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/walletdb"
)

// deriveBoth gives out n more external and n more internal addresses of the
// default account, and returns them.
func deriveBoth(t *testing.T, db walletdb.DB, scoped *ScopedKeyManager,
	n uint32) (external, internal []ManagedAddress) {

	t.Helper()
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		var err error
		external, err = scoped.NextExternalAddresses(ns, DefaultAccountNum, n)
		if err != nil {
			return err
		}
		internal, err = scoped.NextInternalAddresses(ns, DefaultAccountNum, n)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return external, internal
}

// extend calls MaybeExtendAddress for the address.
func extend(t *testing.T, db walletdb.DB, mgr *Manager,
	addr bchutil.Address) ([]bchutil.Address, error) {

	t.Helper()
	var derived []bchutil.Address
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		var err error
		derived, err = mgr.MaybeExtendAddress(ns, addr)
		return err
	})
	return derived, err
}

// TestMaybeExtendAddressKeepsTheGap ensures that an address used with fewer
// than NumInitialAddrs addresses of its branch given out after it extends that
// branch by one, that one used further back doesn't, and that each branch
// counts its own.
func TestMaybeExtendAddressKeepsTheGap(t *testing.T) {
	teardown, db, mgr := setupManager(t)
	defer teardown()
	scoped, err := mgr.FetchScopedKeyManager(KeyScopeBIP0044)
	if err != nil {
		t.Fatal(err)
	}
	external, internal := deriveBoth(t, db, scoped, 30)

	// Far behind the last one given out: the gap is there already.
	derived, err := extend(t, db, mgr, external[5].Address())
	if err != nil || len(derived) != 0 {
		t.Fatalf("an address far back extended: %v, %v", derived, err)
	}

	// Near the end of the external branch: one more of that branch alone.
	derived, err = extend(t, db, mgr, external[25].Address())
	if err != nil || len(derived) != 1 {
		t.Fatalf("an address near the end didn't extend: %v, %v", derived, err)
	}
	var next ManagedAddress
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		addrs, err := scoped.NextInternalAddresses(ns, DefaultAccountNum, 1)
		if err != nil {
			return err
		}
		next = addrs[0]
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, path, _ := next.(ManagedPubKeyAddress).DerivationInfo()
	if path.Index != 30 {
		t.Fatalf("the internal branch moved for an external address: "+
			"next index %d", path.Index)
	}

	// Near the end of the internal branch, which is now 31 long.
	derived, err = extend(t, db, mgr, internal[25].Address())
	if err != nil || len(derived) != 1 {
		t.Fatalf("an internal address near the end didn't extend: %v, %v",
			derived, err)
	}
	if derived[0].String() == next.Address().String() {
		t.Fatal("the internal branch derived an address it had")
	}
}

// TestMaybeExtendAddressCostsTheSame ensures that checking the gap costs about
// the same for an account with many addresses as for one with few, as recovery
// checks it for each address it finds used, and walking the account each time
// made a long history's recovery slow and its memory grow.
func TestMaybeExtendAddressCostsTheSame(t *testing.T) {
	allocs := func(n uint32) float64 {
		teardown, db, mgr := setupManager(t)
		defer teardown()
		scoped, err := mgr.FetchScopedKeyManager(KeyScopeBIP0044)
		if err != nil {
			t.Fatal(err)
		}
		external, _ := deriveBoth(t, db, scoped, n)
		addr := external[0].Address()
		var perCall float64
		err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
			ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
			perCall = testing.AllocsPerRun(20, func() {
				if _, err := mgr.MaybeExtendAddress(ns, addr); err != nil {
					t.Fatal(err)
				}
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return perCall
	}
	few, many := allocs(50), allocs(1000)
	if many > 2*few {
		t.Fatalf("a check made %.0f allocations with 1000 addresses, and "+
			"%.0f with 50", many, few)
	}
}
