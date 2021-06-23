// Copyright (c) 2014-2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package waddrmgr

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/snacl"
	"github.com/gcash/bchwallet/walletdb"
)

// failingCryptoKey is an implementation of the EncryptorDecryptor interface
// with intentionally fails when attempting to encrypt or decrypt with it.
type failingCryptoKey struct {
	cryptoKey
}

// Encrypt intenionally returns a failure when invoked to test error paths.
//
// This is part of the EncryptorDecryptor interface implementation.
func (c *failingCryptoKey) Encrypt(in []byte) ([]byte, error) {
	return nil, errors.New("failed to encrypt")
}

// Decrypt intenionally returns a failure when invoked to test error paths.
//
// This is part of the EncryptorDecryptor interface implementation.
func (c *failingCryptoKey) Decrypt(in []byte) ([]byte, error) {
	return nil, errors.New("failed to decrypt")
}

// newHash converts the passed big-endian hex string into a chainhash.Hash.
// It only differs from the one available in wire in that it panics on an
// error since it will only (and must only) be called with hard-coded, and
// therefore known good, hashes.
func newHash(hexStr string) *chainhash.Hash {
	hash, err := chainhash.NewHashFromStr(hexStr)
	if err != nil {
		panic(err)
	}
	return hash
}

// failingSecretKeyGen is a SecretKeyGenerator that always returns
// snacl.ErrDecryptFailed.
func failingSecretKeyGen(passphrase *[]byte,
	config *ScryptOptions) (*snacl.SecretKey, error) {
	return nil, errors.New("failingSecretKeyGen intentionally failed to decrypt")
}

// testContext is used to store context information about a running test which
// is passed into helper functions.  The useSpends field indicates whether or
// not the spend data should be empty or figure it out based on the specific
// test blocks provided.  This is needed because the first loop where the blocks
// are inserted, the tests are running against the latest block and therefore
// none of the outputs can be spent yet.  However, on subsequent runs, all
// blocks have been inserted and therefore some of the transaction outputs are
// spent.
type testContext struct {
	t            *testing.T
	db           walletdb.DB
	rootManager  *Manager
	manager      *ScopedKeyManager
	account      uint32
	create       bool
	unlocked     bool
	watchingOnly bool
}

// addrType is the type of address being tested
type addrType byte

const (
	addrPubKeyHash addrType = iota
	addrScriptHash
)

// expectedAddr is used to house the expected return values from a managed
// address.  Not all fields for used for all managed address types.
type expectedAddr struct {
	address        string
	addressHash    []byte
	internal       bool
	compressed     bool
	used           bool
	imported       bool
	pubKey         []byte
	privKey        []byte
	privKeyWIF     string
	script         []byte
	derivationInfo DerivationPath
}

// testNamePrefix is a helper to return a prefix to show for test errors based
// on the state of the test context.
func testNamePrefix(tc *testContext) string {
	prefix := "Open "
	if tc.create {
		prefix = "Create "
	}

	return prefix + fmt.Sprintf("account #%d", tc.account)
}

// testManagedPubKeyAddress ensures the data returned by all exported functions
// provided by the passed managed p ublic key address matches the corresponding
// fields in the provided expected address.
//
// When the test context indicates the manager is unlocked, the private data
// will also be tested, otherwise, the functions which deal with private data
// are checked to ensure they return the correct error.
func testManagedPubKeyAddress(tc *testContext, prefix string,
	gotAddr ManagedPubKeyAddress, wantAddr *expectedAddr) bool {

	// Ensure pubkey is the expected value for the managed address.
	var gpubBytes []byte
	if gotAddr.Compressed() {
		gpubBytes = gotAddr.PubKey().SerializeCompressed()
	} else {
		gpubBytes = gotAddr.PubKey().SerializeUncompressed()
	}
	if !reflect.DeepEqual(gpubBytes, wantAddr.pubKey) {
		tc.t.Errorf("%s PubKey: unexpected public key - got %x, want "+
			"%x", prefix, gpubBytes, wantAddr.pubKey)
		return false
	}

	// Ensure exported pubkey string is the expected value for the managed
	// address.
	gpubHex := gotAddr.ExportPubKey()
	wantPubHex := hex.EncodeToString(wantAddr.pubKey)
	if gpubHex != wantPubHex {
		tc.t.Errorf("%s ExportPubKey: unexpected public key - got %s, "+
			"want %s", prefix, gpubHex, wantPubHex)
		return false
	}

	// Ensure that the derivation path has been properly re-set after the
	// address was read from disk.
	_, gotAddrPath, ok := gotAddr.DerivationInfo()
	if !ok && !gotAddr.Imported() {
		tc.t.Errorf("%s PubKey: non-imported address has empty "+
			"derivation info", prefix)
		return false
	}
	expectedDerivationInfo := wantAddr.derivationInfo
	if gotAddrPath != expectedDerivationInfo {
		tc.t.Errorf("%s PubKey: wrong derivation info: expected %v, "+
			"got %v", prefix, spew.Sdump(gotAddrPath),
			spew.Sdump(expectedDerivationInfo))
		return false
	}

	// Ensure private key is the expected value for the managed address.
	// Since this is only available when the manager is unlocked, also check
	// for the expected error when the manager is locked.
	gotPrivKey, err := gotAddr.PrivKey()
	switch {
	case tc.watchingOnly:
		// Confirm expected watching-only error.
		testName := fmt.Sprintf("%s PrivKey", prefix)
		if !checkManagerError(tc.t, testName, err, ErrWatchingOnly) {
			return false
		}
	case tc.unlocked:
		if err != nil {
			tc.t.Errorf("%s PrivKey: unexpected error - got %v",
				prefix, err)
			return false
		}
		gpriv := gotPrivKey.Serialize()
		if !reflect.DeepEqual(gpriv, wantAddr.privKey) {
			tc.t.Errorf("%s PrivKey: unexpected private key - "+
				"got %x, want %x", prefix, gpriv, wantAddr.privKey)
			return false
		}
	default:
		// Confirm expected locked error.
		testName := fmt.Sprintf("%s PrivKey", prefix)
		if !checkManagerError(tc.t, testName, err, ErrLocked) {
			return false
		}
	}

	// Ensure exported private key in Wallet Import Format (WIF) is the
	// expected value for the managed address.  Since this is only available
	// when the manager is unlocked, also check for the expected error when
	// the manager is locked.
	gotWIF, err := gotAddr.ExportPrivKey()
	switch {
	case tc.watchingOnly:
		// Confirm expected watching-only error.
		testName := fmt.Sprintf("%s ExportPrivKey", prefix)
		if !checkManagerError(tc.t, testName, err, ErrWatchingOnly) {
			return false
		}
	case tc.unlocked:
		if err != nil {
			tc.t.Errorf("%s ExportPrivKey: unexpected error - "+
				"got %v", prefix, err)
			return false
		}
		if gotWIF.String() != wantAddr.privKeyWIF {
			tc.t.Errorf("%s ExportPrivKey: unexpected WIF - got "+
				"%v, want %v", prefix, gotWIF.String(),
				wantAddr.privKeyWIF)
			return false
		}
	default:
		// Confirm expected locked error.
		testName := fmt.Sprintf("%s ExportPrivKey", prefix)
		if !checkManagerError(tc.t, testName, err, ErrLocked) {
			return false
		}
	}

	// Imported addresses should return a nil derivation info.
	if _, _, ok := gotAddr.DerivationInfo(); gotAddr.Imported() && ok {
		tc.t.Errorf("%s Imported: expected nil derivation info", prefix)
		return false
	}

	return true
}

// testManagedScriptAddress ensures the data returned by all exported functions
// provided by the passed managed script address matches the corresponding
// fields in the provided expected address.
//
// When the test context indicates the manager is unlocked, the private data
// will also be tested, otherwise, the functions which deal with private data
// are checked to ensure they return the correct error.
func testManagedScriptAddress(tc *testContext, prefix string,
	gotAddr ManagedScriptAddress, wantAddr *expectedAddr) bool {

	// Ensure script is the expected value for the managed address.
	// Ensure script is the expected value for the managed address.  Since
	// this is only available when the manager is unlocked, also check for
	// the expected error when the manager is locked.
	gotScript, err := gotAddr.Script()
	switch {
	case tc.watchingOnly:
		// Confirm expected watching-only error.
		testName := fmt.Sprintf("%s Script", prefix)
		if !checkManagerError(tc.t, testName, err, ErrWatchingOnly) {
			return false
		}
	case tc.unlocked:
		if err != nil {
			tc.t.Errorf("%s Script: unexpected error - got %v",
				prefix, err)
			return false
		}
		if !reflect.DeepEqual(gotScript, wantAddr.script) {
			tc.t.Errorf("%s Script: unexpected script - got %x, "+
				"want %x", prefix, gotScript, wantAddr.script)
			return false
		}
	default:
		// Confirm expected locked error.
		testName := fmt.Sprintf("%s Script", prefix)
		if !checkManagerError(tc.t, testName, err, ErrLocked) {
			return false
		}
	}

	return true
}

// testAddress ensures the data returned by all exported functions provided by
// the passed managed address matches the corresponding fields in the provided
// expected address.  It also type asserts the managed address to determine its
// specific type and calls the corresponding testing functions accordingly.
//
// When the test context indicates the manager is unlocked, the private data
// will also be tested, otherwise, the functions which deal with private data
// are checked to ensure they return the correct error.
func testAddress(tc *testContext, prefix string, gotAddr ManagedAddress,
	wantAddr *expectedAddr) bool {

	if gotAddr.Account() != tc.account {
		tc.t.Errorf("ManagedAddress.Account: unexpected account - got "+
			"%d, want %d", gotAddr.Account(), tc.account)
		return false
	}

	if gotAddr.Address().EncodeAddress() != wantAddr.address {
		tc.t.Errorf("%s EncodeAddress: unexpected address - got %s, "+
			"want %s", prefix, gotAddr.Address().EncodeAddress(),
			wantAddr.address)
		return false
	}

	if !reflect.DeepEqual(gotAddr.AddrHash(), wantAddr.addressHash) {
		tc.t.Errorf("%s AddrHash: unexpected address hash - got %x, "+
			"want %x", prefix, gotAddr.AddrHash(),
			wantAddr.addressHash)
		return false
	}

	if gotAddr.Internal() != wantAddr.internal {
		tc.t.Errorf("%s Internal: unexpected internal flag - got %v, "+
			"want %v", prefix, gotAddr.Internal(), wantAddr.internal)
		return false
	}

	if gotAddr.Compressed() != wantAddr.compressed {
		tc.t.Errorf("%s Compressed: unexpected compressed flag - got "+
			"%v, want %v", prefix, gotAddr.Compressed(),
			wantAddr.compressed)
		return false
	}

	if gotAddr.Imported() != wantAddr.imported {
		tc.t.Errorf("%s Imported: unexpected imported flag - got %v, "+
			"want %v", prefix, gotAddr.Imported(), wantAddr.imported)
		return false
	}

	switch addr := gotAddr.(type) {
	case ManagedPubKeyAddress:
		if !testManagedPubKeyAddress(tc, prefix, addr, wantAddr) {
			return false
		}

	case ManagedScriptAddress:
		if !testManagedScriptAddress(tc, prefix, addr, wantAddr) {
			return false
		}
	}

	return true
}

// testExternalAddresses tests several facets of external addresses such as
// generating multiple addresses via NextExternalAddresses, ensuring they can be
// retrieved by Address, and that they work properly when the manager is locked
// and unlocked.
func testExternalAddresses(tc *testContext) bool {
	prefix := testNamePrefix(tc) + " testExternalAddresses"
	var addrs []ManagedAddress
	if tc.create {
		prefix := prefix + " NextExternalAddresses"
		var addrs []ManagedAddress
		err := walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
			ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
			var err error
			addrs, err = tc.manager.NextExternalAddresses(ns, tc.account, 5)
			return err
		})
		if err != nil {
			tc.t.Errorf("%s: unexpected error: %v", prefix, err)
			return false
		}
		if len(addrs) != len(expectedExternalAddrs) {
			tc.t.Errorf("%s: unexpected number of addresses - got "+
				"%d, want %d", prefix, len(addrs),
				len(expectedExternalAddrs))
			return false
		}
	}

	// Setup a closure to test the results since the same tests need to be
	// repeated with the manager locked and unlocked.
	testResults := func() bool {
		// Ensure the returned addresses are the expected ones.  When
		// not in the create phase, there will be no addresses in the
		// addrs slice, so this really only runs during the first phase
		// of the tests.
		for i := 0; i < len(addrs); i++ {
			prefix := fmt.Sprintf("%s ExternalAddress #%d", prefix, i)
			if !testAddress(tc, prefix, addrs[i], &expectedExternalAddrs[i]) {
				return false
			}
		}

		// Ensure the last external address is the expected one.
		leaPrefix := prefix + " LastExternalAddress"
		var lastAddr ManagedAddress
		err := walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
			ns := tx.ReadBucket(waddrmgrNamespaceKey)
			var err error
			lastAddr, err = tc.manager.LastExternalAddress(ns, tc.account)
			return err
		})
		if err != nil {
			tc.t.Errorf("%s: unexpected error: %v", leaPrefix, err)
			return false
		}
		if !testAddress(tc, leaPrefix, lastAddr, &expectedExternalAddrs[len(expectedExternalAddrs)-1]) {
			return false
		}

		// Now, use the Address API to retrieve each of the expected new
		// addresses and ensure they're accurate.
		chainParams := tc.manager.ChainParams()
		for i := 0; i < len(expectedExternalAddrs); i++ {
			pkHash := expectedExternalAddrs[i].addressHash
			utilAddr, err := bchutil.NewAddressPubKeyHash(
				pkHash, chainParams,
			)
			if err != nil {
				tc.t.Errorf("%s NewAddressPubKeyHash #%d: "+
					"unexpected error: %v", prefix, i, err)
				return false
			}

			prefix := fmt.Sprintf("%s Address #%d", prefix, i)
			var addr ManagedAddress
			err = walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
				ns := tx.ReadBucket(waddrmgrNamespaceKey)
				var err error
				addr, err = tc.manager.Address(ns, utilAddr)
				return err
			})
			if err != nil {
				tc.t.Errorf("%s: unexpected error: %v", prefix,
					err)
				return false
			}

			if !testAddress(tc, prefix, addr, &expectedExternalAddrs[i]) {
				return false
			}
		}

		return true
	}

	// Since the manager is locked at this point, the public address
	// information is tested and the private functions are checked to ensure
	// they return the expected error.
	if !testResults() {
		return false
	}

	// Everything after this point involves retesting with an unlocked
	// address manager which is not possible for watching-only mode, so
	// just exit now in that case.
	if tc.watchingOnly {
		return true
	}

	// Unlock the manager and retest all of the addresses to ensure the
	// private information is valid as well.
	err := walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		return tc.rootManager.Unlock(ns, privPassphrase)
	})
	if err != nil {
		tc.t.Errorf("Unlock: unexpected error: %v", err)
		return false
	}
	tc.unlocked = true
	if !testResults() {
		return false
	}

	// Relock the manager for future tests.
	if err := tc.rootManager.Lock(); err != nil {
		tc.t.Errorf("Lock: unexpected error: %v", err)
		return false
	}
	tc.unlocked = false

	return true
}

// testInternalAddresses tests several facets of internal addresses such as
// generating multiple addresses via NextInternalAddresses, ensuring they can be
// retrieved by Address, and that they work properly when the manager is locked
// and unlocked.
func testInternalAddresses(tc *testContext) bool {
	// When the address manager is not in watching-only mode, unlocked it
	// first to ensure that address generation works correctly when the
	// address manager is unlocked and then locked later.  These tests
	// reverse the order done in the external tests which starts with a
	// locked manager and unlock it afterwards.
	if !tc.watchingOnly {
		// Unlock the manager and retest all of the addresses to ensure the
		// private information is valid as well.
		err := walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
			ns := tx.ReadBucket(waddrmgrNamespaceKey)
			return tc.rootManager.Unlock(ns, privPassphrase)
		})
		if err != nil {
			tc.t.Errorf("Unlock: unexpected error: %v", err)
			return false
		}
		tc.unlocked = true
	}

	prefix := testNamePrefix(tc) + " testInternalAddresses"
	var addrs []ManagedAddress
	if tc.create {
		prefix := prefix + " NextInternalAddress"
		err := walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
			ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
			var err error
			addrs, err = tc.manager.NextInternalAddresses(ns, tc.account, 5)
			return err
		})
		if err != nil {
			tc.t.Errorf("%s: unexpected error: %v", prefix, err)
			return false
		}
		if len(addrs) != len(expectedInternalAddrs) {
			tc.t.Errorf("%s: unexpected number of addresses - got "+
				"%d, want %d", prefix, len(addrs),
				len(expectedInternalAddrs))
			return false
		}
	}

	// Setup a closure to test the results since the same tests need to be
	// repeated with the manager locked and unlocked.
	testResults := func() bool {
		// Ensure the returned addresses are the expected ones.  When
		// not in the create phase, there will be no addresses in the
		// addrs slice, so this really only runs during the first phase
		// of the tests.
		for i := 0; i < len(addrs); i++ {
			prefix := fmt.Sprintf("%s InternalAddress #%d", prefix, i)
			if !testAddress(tc, prefix, addrs[i], &expectedInternalAddrs[i]) {
				return false
			}
		}

		// Ensure the last internal address is the expected one.
		liaPrefix := prefix + " LastInternalAddress"
		var lastAddr ManagedAddress
		err := walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
			ns := tx.ReadBucket(waddrmgrNamespaceKey)
			var err error
			lastAddr, err = tc.manager.LastInternalAddress(ns, tc.account)
			return err
		})
		if err != nil {
			tc.t.Errorf("%s: unexpected error: %v", liaPrefix, err)
			return false
		}
		if !testAddress(tc, liaPrefix, lastAddr, &expectedInternalAddrs[len(expectedInternalAddrs)-1]) {
			return false
		}

		// Now, use the Address API to retrieve each of the expected new
		// addresses and ensure they're accurate.
		chainParams := tc.manager.ChainParams()
		for i := 0; i < len(expectedInternalAddrs); i++ {
			pkHash := expectedInternalAddrs[i].addressHash
			utilAddr, err := bchutil.NewAddressPubKeyHash(
				pkHash, chainParams,
			)
			if err != nil {
				tc.t.Errorf("%s NewAddressPubKeyHash #%d: "+
					"unexpected error: %v", prefix, i, err)
				return false
			}

			prefix := fmt.Sprintf("%s Address #%d", prefix, i)
			var addr ManagedAddress
			err = walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
				ns := tx.ReadBucket(waddrmgrNamespaceKey)
				var err error
				addr, err = tc.manager.Address(ns, utilAddr)
				return err
			})
			if err != nil {
				tc.t.Errorf("%s: unexpected error: %v", prefix,
					err)
				return false
			}

			if !testAddress(tc, prefix, addr, &expectedInternalAddrs[i]) {
				return false
			}
		}

		return true
	}

	// The address manager could either be locked or unlocked here depending
	// on whether or not it's a watching-only manager.  When it's unlocked,
	// this will test both the public and private address data are accurate.
	// When it's locked, it must be watching-only, so only the public
	// address information is tested and the private functions are checked
	// to ensure they return the expected ErrWatchingOnly error.
	if !testResults() {
		return false
	}

	// Everything after this point involves locking the address manager and
	// retesting the addresses with a locked manager.  However, for
	// watching-only mode, this has already happened, so just exit now in
	// that case.
	if tc.watchingOnly {
		return true
	}

	// Lock the manager and retest all of the addresses to ensure the
	// public information remains valid and the private functions return
	// the expected error.
	if err := tc.rootManager.Lock(); err != nil {
		tc.t.Errorf("Lock: unexpected error: %v", err)
		return false
	}
	tc.unlocked = false
	return testResults()
}

// testLocking tests the basic locking semantics of the address manager work
// as expected.  Other tests ensure addresses behave as expected under locked
// and unlocked conditions.
func testLocking(tc *testContext) bool {
	if tc.unlocked {
		tc.t.Error("testLocking called with an unlocked manager")
		return false
	}
	if !tc.rootManager.IsLocked() {
		tc.t.Error("IsLocked: returned false on locked manager")
		return false
	}

	// Locking an already lock manager should return an error.  The error
	// should be ErrLocked or ErrWatchingOnly depending on the type of the
	// address manager.
	err := tc.rootManager.Lock()
	wantErrCode := ErrLocked
	if tc.watchingOnly {
		wantErrCode = ErrWatchingOnly
	}
	if !checkManagerError(tc.t, "Lock", err, wantErrCode) {
		return false
	}

	// Ensure unlocking with the correct passphrase doesn't return any
	// unexpected errors and the manager properly reports it is unlocked.
	// Since watching-only address managers can't be unlocked, also ensure
	// the correct error for that case.
	err = walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		return tc.rootManager.Unlock(ns, privPassphrase)
	})
	if tc.watchingOnly {
		if !checkManagerError(tc.t, "Unlock", err, ErrWatchingOnly) {
			return false
		}
	} else if err != nil {
		tc.t.Errorf("Unlock: unexpected error: %v", err)
		return false
	}
	if !tc.watchingOnly && tc.rootManager.IsLocked() {
		tc.t.Error("IsLocked: returned true on unlocked manager")
		return false
	}

	// Unlocking the manager again is allowed.  Since watching-only address
	// managers can't be unlocked, also ensure the correct error for that
	// case.
	err = walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		return tc.rootManager.Unlock(ns, privPassphrase)
	})
	if tc.watchingOnly {
		if !checkManagerError(tc.t, "Unlock2", err, ErrWatchingOnly) {
			return false
		}
	} else if err != nil {
		tc.t.Errorf("Unlock: unexpected error: %v", err)
		return false
	}
	if !tc.watchingOnly && tc.rootManager.IsLocked() {
		tc.t.Error("IsLocked: returned true on unlocked manager")
		return false
	}

	// Unlocking the manager with an invalid passphrase must result in an
	// error and a locked manager.
	err = walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		return tc.rootManager.Unlock(ns, []byte("invalidpassphrase"))
	})
	wantErrCode = ErrWrongPassphrase
	if tc.watchingOnly {
		wantErrCode = ErrWatchingOnly
	}
	if !checkManagerError(tc.t, "Unlock", err, wantErrCode) {
		return false
	}
	if !tc.rootManager.IsLocked() {
		tc.t.Error("IsLocked: manager is unlocked after failed unlock " +
			"attempt")
		return false
	}

	return true
}

// testImportPrivateKey tests that importing private keys works properly.  It
// ensures they can be retrieved by Address after they have been imported and
// the addresses give the expected values when the manager is locked and
// unlocked.
//
// This function expects the manager is already locked when called and returns
// with the manager locked.
func testImportPrivateKey(tc *testContext) bool {
	tests := []struct {
		name       string
		in         string
		blockstamp BlockStamp
		expected   expectedAddr
	}{
		{
			name: "wif for uncompressed pubkey address",
			in:   "5HueCGU8rMjxEXxiPuD5BDku4MkFqeZyd4dZ1jvhTVqvbTLvyTJ",
			expected: expectedAddr{
				address:     "qzn96x3rn48vveny856sc7acl3zd9zq39qgcr5u4g9",
				addressHash: hexToBytes("a65d1a239d4ec666643d350c7bb8fc44d2881128"),
				internal:    false,
				imported:    true,
				compressed:  false,
				pubKey: hexToBytes("04d0de0aaeaefad02b8bdc8a01a1b8b11c696bd3" +
					"d66a2c5f10780d95b7df42645cd85228a6fb29940e858e7e558" +
					"42ae2bd115d1ed7cc0e82d934e929c97648cb0a"),
				privKey: hexToBytes("0c28fca386c7a227600b2fe50b7cae11ec86d3bf1fbe471be89827e19d72aa1d"),
				// privKeyWIF is set to the in field during tests
			},
		},
		{
			name: "wif for compressed pubkey address",
			in:   "KwdMAjGmerYanjeui5SHS7JkmpZvVipYvB2LJGU1ZxJwYvP98617",
			expected: expectedAddr{
				address:     "qrvn28wt44dc7wutlghjehy9c2q33j5nyctecw50xm",
				addressHash: hexToBytes("d9351dcbad5b8f3b8bfa2f2cdc85c28118ca9326"),
				internal:    false,
				imported:    true,
				compressed:  true,
				pubKey:      hexToBytes("02d0de0aaeaefad02b8bdc8a01a1b8b11c696bd3d66a2c5f10780d95b7df42645c"),
				privKey:     hexToBytes("0c28fca386c7a227600b2fe50b7cae11ec86d3bf1fbe471be89827e19d72aa1d"),
				// privKeyWIF is set to the in field during tests
			},
		},
	}

	// The manager must be unlocked to import a private key, however a
	// watching-only manager can't be unlocked.
	if !tc.watchingOnly {
		err := walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
			ns := tx.ReadBucket(waddrmgrNamespaceKey)
			return tc.rootManager.Unlock(ns, privPassphrase)
		})
		if err != nil {
			tc.t.Errorf("Unlock: unexpected error: %v", err)
			return false
		}
		tc.unlocked = true
	}

	// Only import the private keys when in the create phase of testing.
	tc.account = ImportedAddrAccount
	prefix := testNamePrefix(tc) + " testImportPrivateKey"
	if tc.create {
		for i, test := range tests {
			test.expected.privKeyWIF = test.in
			wif, err := bchutil.DecodeWIF(test.in)
			if err != nil {
				tc.t.Errorf("%s DecodeWIF #%d (%s): unexpected "+
					"error: %v", prefix, i, test.name, err)
				continue
			}
			var addr ManagedPubKeyAddress
			err = walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
				ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
				var err error
				addr, err = tc.manager.ImportPrivateKey(ns, wif, &test.blockstamp)
				return err
			})
			if err != nil {
				tc.t.Errorf("%s ImportPrivateKey #%d (%s): "+
					"unexpected error: %v", prefix, i,
					test.name, err)
				continue
			}
			if !testAddress(tc, prefix+" ImportPrivateKey", addr,
				&test.expected) {
				continue
			}
		}
	}

	// Setup a closure to test the results since the same tests need to be
	// repeated with the manager unlocked and locked.
	chainParams := tc.manager.ChainParams()
	testResults := func() bool {
		failed := false
		for i, test := range tests {
			test.expected.privKeyWIF = test.in

			// Use the Address API to retrieve each of the expected
			// new addresses and ensure they're accurate.
			utilAddr, err := bchutil.NewAddressPubKeyHash(
				test.expected.addressHash, chainParams)
			if err != nil {
				tc.t.Errorf("%s NewAddressPubKeyHash #%d (%s): "+
					"unexpected error: %v", prefix, i,
					test.name, err)
				failed = true
				continue
			}
			taPrefix := fmt.Sprintf("%s Address #%d (%s)", prefix,
				i, test.name)
			var ma ManagedAddress
			err = walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
				ns := tx.ReadBucket(waddrmgrNamespaceKey)
				var err error
				ma, err = tc.manager.Address(ns, utilAddr)
				return err
			})
			if err != nil {
				tc.t.Errorf("%s: unexpected error: %v", taPrefix,
					err)
				failed = true
				continue
			}
			if !testAddress(tc, taPrefix, ma, &test.expected) {
				failed = true
				continue
			}
		}

		return !failed
	}

	// The address manager could either be locked or unlocked here depending
	// on whether or not it's a watching-only manager.  When it's unlocked,
	// this will test both the public and private address data are accurate.
	// When it's locked, it must be watching-only, so only the public
	// address  information is tested and the private functions are checked
	// to ensure they return the expected ErrWatchingOnly error.
	if !testResults() {
		return false
	}

	// Everything after this point involves locking the address manager and
	// retesting the addresses with a locked manager.  However, for
	// watching-only mode, this has already happened, so just exit now in
	// that case.
	if tc.watchingOnly {
		return true
	}

	// Lock the manager and retest all of the addresses to ensure the
	// private information returns the expected error.
	if err := tc.rootManager.Lock(); err != nil {
		tc.t.Errorf("Lock: unexpected error: %v", err)
		return false
	}
	tc.unlocked = false
	return testResults()
}

// testImportScript tests that importing scripts works properly.  It ensures
// they can be retrieved by Address after they have been imported and the
// addresses give the expected values when the manager is locked and unlocked.
//
// This function expects the manager is already locked when called and returns
// with the manager locked.
func testImportScript(tc *testContext) bool {
	tests := []struct {
		name       string
		in         []byte
		blockstamp BlockStamp
		expected   expectedAddr
	}{
		{
			name: "p2sh uncompressed pubkey",
			in: hexToBytes("41048b65a0e6bb200e6dac05e74281b1ab9a41e8" +
				"0006d6b12d8521e09981da97dd96ac72d24d1a7d" +
				"ed9493a9fc20fdb4a714808f0b680f1f1d935277" +
				"48b5e3f629ffac"),
			expected: expectedAddr{
				address:     "prdxu6nr9ktdc4fs67eunucpwujaqgcf8cd74ct5wp",
				addressHash: hexToBytes("da6e6a632d96dc5530d7b3c9f3017725d023093e"),
				internal:    false,
				imported:    true,
				compressed:  false,
				// script is set to the in field during tests.
			},
		},
		{
			name: "p2sh multisig",
			in: hexToBytes("524104cb9c3c222c5f7a7d3b9bd152f363a0b6d5" +
				"4c9eb312c4d4f9af1e8551b6c421a6a4ab0e2910" +
				"5f24de20ff463c1c91fcf3bf662cdde4783d4799" +
				"f787cb7c08869b4104ccc588420deeebea22a7e9" +
				"00cc8b68620d2212c374604e3487ca08f1ff3ae1" +
				"2bdc639514d0ec8612a2d3c519f084d9a00cbbe3" +
				"b53d071e9b09e71e610b036aa24104ab47ad1939" +
				"edcb3db65f7fedea62bbf781c5410d3f22a7a3a5" +
				"6ffefb2238af8627363bdf2ed97c1f89784a1aec" +
				"db43384f11d2acc64443c7fc299cef0400421a53ae"),
			expected: expectedAddr{
				address:     "pqdcqr8vrl5jyghndfgzcyuma4rut9vhz5peun2far",
				addressHash: hexToBytes("1b800cec1fe92222f36a502c139bed47c5959715"),
				internal:    false,
				imported:    true,
				compressed:  false,
				// script is set to the in field during tests.
			},
		},
	}

	// The manager must be unlocked to import a private key and also for
	// testing private data.  However, a watching-only manager can't be
	// unlocked.
	if !tc.watchingOnly {
		err := walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
			ns := tx.ReadBucket(waddrmgrNamespaceKey)
			return tc.rootManager.Unlock(ns, privPassphrase)
		})
		if err != nil {
			tc.t.Errorf("Unlock: unexpected error: %v", err)
			return false
		}
		tc.unlocked = true
	}

	// Only import the scripts when in the create phase of testing.
	tc.account = ImportedAddrAccount
	prefix := testNamePrefix(tc)
	if tc.create {
		for i, test := range tests {
			test.expected.script = test.in
			prefix := fmt.Sprintf("%s ImportScript #%d (%s)", prefix,
				i, test.name)

			var addr ManagedScriptAddress
			err := walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
				ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
				var err error
				addr, err = tc.manager.ImportScript(ns, test.in, &test.blockstamp)
				return err
			})
			if err != nil {
				tc.t.Errorf("%s: unexpected error: %v", prefix,
					err)
				continue
			}
			if !testAddress(tc, prefix, addr, &test.expected) {
				continue
			}
		}
	}

	// Setup a closure to test the results since the same tests need to be
	// repeated with the manager unlocked and locked.
	chainParams := tc.manager.ChainParams()
	testResults := func() bool {
		failed := false
		for i, test := range tests {
			test.expected.script = test.in

			// Use the Address API to retrieve each of the expected
			// new addresses and ensure they're accurate.
			utilAddr, err := bchutil.NewAddressScriptHash(test.in,
				chainParams)
			if err != nil {
				tc.t.Errorf("%s NewAddressScriptHash #%d (%s): "+
					"unexpected error: %v", prefix, i,
					test.name, err)
				failed = true
				continue
			}
			taPrefix := fmt.Sprintf("%s Address #%d (%s)", prefix,
				i, test.name)
			var ma ManagedAddress
			err = walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
				ns := tx.ReadBucket(waddrmgrNamespaceKey)
				var err error
				ma, err = tc.manager.Address(ns, utilAddr)
				return err
			})
			if err != nil {
				tc.t.Errorf("%s: unexpected error: %v", taPrefix,
					err)
				failed = true
				continue
			}
			if !testAddress(tc, taPrefix, ma, &test.expected) {
				failed = true
				continue
			}
		}

		return !failed
	}

	// The address manager could either be locked or unlocked here depending
	// on whether or not it's a watching-only manager.  When it's unlocked,
	// this will test both the public and private address data are accurate.
	// When it's locked, it must be watching-only, so only the public
	// address information is tested and the private functions are checked
	// to ensure they return the expected ErrWatchingOnly error.
	if !testResults() {
		return false
	}

	// Everything after this point involves locking the address manager and
	// retesting the addresses with a locked manager.  However, for
	// watching-only mode, this has already happened, so just exit now in
	// that case.
	if tc.watchingOnly {
		return true
	}

	// Lock the manager and retest all of the addresses to ensure the
	// private information returns the expected error.
	if err := tc.rootManager.Lock(); err != nil {
		tc.t.Errorf("Lock: unexpected error: %v", err)
		return false
	}
	tc.unlocked = false
	return testResults()
}

// testMarkUsed ensures used addresses are flagged as such.
func testMarkUsed(tc *testContext) bool {
	tests := []struct {
		name string
		typ  addrType
		in   []byte
	}{
		{
			name: "managed address",
			typ:  addrPubKeyHash,
			in:   hexToBytes("2ef94abb9ee8f785d087c3ec8d6ee467e92d0d0a"),
		},
		{
			name: "script address",
			typ:  addrScriptHash,
			in:   hexToBytes("da6e6a632d96dc5530d7b3c9f3017725d023093e"),
		},
	}

	prefix := "MarkUsed"
	chainParams := tc.manager.ChainParams()
	for i, test := range tests {
		addrHash := test.in

		var addr bchutil.Address
		var err error
		switch test.typ {
		case addrPubKeyHash:
			addr, err = bchutil.NewAddressPubKeyHash(addrHash, chainParams)
		case addrScriptHash:
			addr, err = bchutil.NewAddressScriptHashFromHash(addrHash, chainParams)
		default:
			panic("unreachable")
		}
		if err != nil {
			tc.t.Errorf("%s #%d: NewAddress unexpected error: %v", prefix, i, err)
			continue
		}

		err = walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
			ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)

			maddr, err := tc.manager.Address(ns, addr)
			if err != nil {
				tc.t.Errorf("%s #%d: Address unexpected error: %v", prefix, i, err)
				return nil
			}
			if tc.create {
				// Test that initially the address is not flagged as used
				used := maddr.Used(ns)
				if used {
					tc.t.Errorf("%s #%d: unexpected used flag -- got "+
						"%v, want %v", prefix, i, used, false)
				}
			}
			err = tc.manager.MarkUsed(ns, addr)
			if err != nil {
				tc.t.Errorf("%s #%d: unexpected error: %v", prefix, i, err)
				return nil
			}
			used := maddr.Used(ns)
			if !used {
				tc.t.Errorf("%s #%d: unexpected used flag -- got "+
					"%v, want %v", prefix, i, used, true)
			}
			return nil
		})
		if err != nil {
			tc.t.Errorf("Unexpected error %v", err)
		}
	}

	return true
}

// testChangePassphrase ensures changes both the public and private passphrases
// works as intended.
func testChangePassphrase(tc *testContext) bool {
	// Attempt to change public passphrase with invalid old passphrase.
	testName := "ChangePassphrase (public) with invalid old passphrase"
	err := walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		return tc.rootManager.ChangePassphrase(
			ns, []byte("bogus"), pubPassphrase2, false, fastScrypt,
		)
	})
	if !checkManagerError(tc.t, testName, err, ErrWrongPassphrase) {
		return false
	}

	// Change the public passphrase.
	testName = "ChangePassphrase (public)"
	err = walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		return tc.rootManager.ChangePassphrase(
			ns, pubPassphrase, pubPassphrase2, false, fastScrypt,
		)
	})
	if err != nil {
		tc.t.Errorf("%s: unexpected error: %v", testName, err)
		return false
	}

	// Ensure the public passphrase was successfully changed. We do this by
	// being able to re-derive the public key with the new passphrase.
	secretKey := snacl.SecretKey{Key: &snacl.CryptoKey{}}
	secretKey.Parameters = tc.rootManager.masterKeyPub.Parameters
	if err := secretKey.DeriveKey(&pubPassphrase2); err != nil {
		tc.t.Errorf("%s: passphrase does not match", testName)
		return false
	}

	// Change the private passphrase back to what it was.
	err = walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		return tc.rootManager.ChangePassphrase(
			ns, pubPassphrase2, pubPassphrase, false, fastScrypt,
		)
	})
	if err != nil {
		tc.t.Errorf("%s: unexpected error: %v", testName, err)
		return false
	}

	// Attempt to change private passphrase with invalid old passphrase.
	// The error should be ErrWrongPassphrase or ErrWatchingOnly depending
	// on the type of the address manager.
	testName = "ChangePassphrase (private) with invalid old passphrase"
	err = walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		return tc.rootManager.ChangePassphrase(
			ns, []byte("bogus"), privPassphrase2, true, fastScrypt,
		)
	})
	wantErrCode := ErrWrongPassphrase
	if tc.watchingOnly {
		wantErrCode = ErrWatchingOnly
	}
	if !checkManagerError(tc.t, testName, err, wantErrCode) {
		return false
	}

	// Everything after this point involves testing that the private
	// passphrase for the address manager can be changed successfully.
	// This is not possible for watching-only mode, so just exit now in that
	// case.
	if tc.watchingOnly {
		return true
	}

	// Change the private passphrase.
	testName = "ChangePassphrase (private)"
	err = walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		return tc.rootManager.ChangePassphrase(
			ns, privPassphrase, privPassphrase2, true, fastScrypt,
		)
	})
	if err != nil {
		tc.t.Errorf("%s: unexpected error: %v", testName, err)
		return false
	}

	// Unlock the manager with the new passphrase to ensure it changed as
	// expected.
	err = walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		return tc.rootManager.Unlock(ns, privPassphrase2)
	})
	if err != nil {
		tc.t.Errorf("%s: failed to unlock with new private "+
			"passphrase: %v", testName, err)
		return false
	}
	tc.unlocked = true

	// Change the private passphrase back to what it was while the manager
	// is unlocked to ensure that path works properly as well.
	err = walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		return tc.rootManager.ChangePassphrase(
			ns, privPassphrase2, privPassphrase, true, fastScrypt,
		)
	})
	if err != nil {
		tc.t.Errorf("%s: unexpected error: %v", testName, err)
		return false
	}
	if tc.rootManager.IsLocked() {
		tc.t.Errorf("%s: manager is locked", testName)
		return false
	}

	// Relock the manager for future tests.
	if err := tc.rootManager.Lock(); err != nil {
		tc.t.Errorf("Lock: unexpected error: %v", err)
		return false
	}
	tc.unlocked = false

	return true
}

// testNewAccount tests the new account creation func of the address manager works
// as expected.
func testNewAccount(tc *testContext) bool {
	if tc.watchingOnly {
		// Creating new accounts in watching-only mode should return ErrWatchingOnly
		err := walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
			ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
			_, err := tc.manager.NewAccount(ns, "test")
			return err
		})
		if !checkManagerError(
			tc.t, "Create account in watching-only mode", err,
			ErrWatchingOnly,
		) {
			tc.manager.Close()
			return false
		}
		return true
	}
	// Creating new accounts when wallet is locked should return ErrLocked
	err := walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		_, err := tc.manager.NewAccount(ns, "test")
		return err
	})
	if !checkManagerError(
		tc.t, "Create account when wallet is locked", err, ErrLocked,
	) {
		tc.manager.Close()
		return false
	}
	// Unlock the wallet to decrypt cointype keys required
	// to derive account keys
	err = walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		err := tc.rootManager.Unlock(ns, privPassphrase)
		return err
	})
	if err != nil {
		tc.t.Errorf("Unlock: unexpected error: %v", err)
		return false
	}
	tc.unlocked = true

	testName := "acct-create"
	expectedAccount := tc.account + 1
	if !tc.create {
		// Create a new account in open mode
		testName = "acct-open"
		expectedAccount++
	}
	var account uint32
	err = walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		var err error
		account, err = tc.manager.NewAccount(ns, testName)
		return err
	})
	if err != nil {
		tc.t.Errorf("NewAccount: unexpected error: %v", err)
		return false
	}
	if account != expectedAccount {
		tc.t.Errorf("NewAccount "+
			"account mismatch -- got %d, "+
			"want %d", account, expectedAccount)
		return false
	}

	// Test duplicate account name error
	err = walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		_, err := tc.manager.NewAccount(ns, testName)
		return err
	})
	wantErrCode := ErrDuplicateAccount
	if !checkManagerError(tc.t, testName, err, wantErrCode) {
		return false
	}
	// Test account name validation
	testName = "" // Empty account names are not allowed
	err = walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		_, err := tc.manager.NewAccount(ns, testName)
		return err
	})
	wantErrCode = ErrInvalidAccount
	if !checkManagerError(tc.t, testName, err, wantErrCode) {
		return false
	}
	testName = "imported" // A reserved account name
	err = walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		_, err := tc.manager.NewAccount(ns, testName)
		return err
	})
	wantErrCode = ErrInvalidAccount
	return checkManagerError(tc.t, testName, err, wantErrCode)
}

// testLookupAccount tests the basic account lookup func of the address manager
// works as expected.
func testLookupAccount(tc *testContext) bool {
	// Lookup accounts created earlier in testNewAccount
	expectedAccounts := map[string]uint32{
		defaultAccountName:      DefaultAccountNum,
		ImportedAddrAccountName: ImportedAddrAccount,
	}
	for acctName, expectedAccount := range expectedAccounts {
		var account uint32
		err := walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
			ns := tx.ReadBucket(waddrmgrNamespaceKey)
			var err error
			account, err = tc.manager.LookupAccount(ns, acctName)
			return err
		})
		if err != nil {
			tc.t.Errorf("LookupAccount: unexpected error: %v", err)
			return false
		}
		if account != expectedAccount {
			tc.t.Errorf("LookupAccount "+
				"account mismatch -- got %d, "+
				"want %d", account, expectedAccount)
			return false
		}
	}
	// Test account not found error
	testName := "non existent account"
	err := walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		_, err := tc.manager.LookupAccount(ns, testName)
		return err
	})
	wantErrCode := ErrAccountNotFound
	if !checkManagerError(tc.t, testName, err, wantErrCode) {
		return false
	}

	// Test last account
	var lastAccount uint32
	err = walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		var err error
		lastAccount, err = tc.manager.LastAccount(ns)
		return err
	})
	expectedLastAccount := uint32(1)
	if !tc.create {
		// Existing wallet manager will have 3 accounts
		expectedLastAccount = 2
	}
	if lastAccount != expectedLastAccount {
		tc.t.Errorf("LookupAccount "+
			"account mismatch -- got %d, "+
			"want %d", lastAccount, expectedLastAccount)
		return false
	}

	// Test account lookup for default account adddress
	var expectedAccount uint32
	for i, addr := range expectedAddrs {
		addr, err := bchutil.NewAddressPubKeyHash(addr.addressHash,
			tc.manager.ChainParams())
		if err != nil {
			tc.t.Errorf("AddrAccount #%d: unexpected error: %v", i, err)
			return false
		}
		var account uint32
		err = walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
			ns := tx.ReadBucket(waddrmgrNamespaceKey)
			var err error
			account, err = tc.manager.AddrAccount(ns, addr)
			return err
		})
		if err != nil {
			tc.t.Errorf("AddrAccount #%d: unexpected error: %v", i, err)
			return false
		}
		if account != expectedAccount {
			tc.t.Errorf("AddrAccount "+
				"account mismatch -- got %d, "+
				"want %d", account, expectedAccount)
			return false
		}
	}
	return true
}

// testRenameAccount tests the rename account func of the address manager works
// as expected.
func testRenameAccount(tc *testContext) bool {
	var acctName string
	err := walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		var err error
		acctName, err = tc.manager.AccountName(ns, tc.account)
		return err
	})
	if err != nil {
		tc.t.Errorf("AccountName: unexpected error: %v", err)
		return false
	}
	testName := acctName + "-renamed"
	err = walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		return tc.manager.RenameAccount(ns, tc.account, testName)
	})
	if err != nil {
		tc.t.Errorf("RenameAccount: unexpected error: %v", err)
		return false
	}
	var newName string
	err = walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		var err error
		newName, err = tc.manager.AccountName(ns, tc.account)
		return err
	})
	if err != nil {
		tc.t.Errorf("AccountName: unexpected error: %v", err)
		return false
	}
	if newName != testName {
		tc.t.Errorf("RenameAccount "+
			"account name mismatch -- got %s, "+
			"want %s", newName, testName)
		return false
	}
	// Test duplicate account name error
	err = walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		return tc.manager.RenameAccount(ns, tc.account, testName)
	})
	wantErrCode := ErrDuplicateAccount
	if !checkManagerError(tc.t, testName, err, wantErrCode) {
		return false
	}
	// Test old account name is no longer valid
	err = walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		_, err := tc.manager.LookupAccount(ns, acctName)
		return err
	})
	wantErrCode = ErrAccountNotFound
	return checkManagerError(tc.t, testName, err, wantErrCode)
}

// testForEachAccount tests the retrieve all accounts func of the address
// manager works as expected.
func testForEachAccount(tc *testContext) bool {
	prefix := testNamePrefix(tc) + " testForEachAccount"
	expectedAccounts := []uint32{0, 1}
	if !tc.create {
		// Existing wallet manager will have 3 accounts
		expectedAccounts = append(expectedAccounts, 2)
	}
	// Imported account
	expectedAccounts = append(expectedAccounts, ImportedAddrAccount)
	var accounts []uint32
	err := walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		return tc.manager.ForEachAccount(ns, func(account uint32) error {
			accounts = append(accounts, account)
			return nil
		})
	})
	if err != nil {
		tc.t.Errorf("%s: unexpected error: %v", prefix, err)
		return false
	}
	if len(accounts) != len(expectedAccounts) {
		tc.t.Errorf("%s: unexpected number of accounts - got "+
			"%d, want %d", prefix, len(accounts),
			len(expectedAccounts))
		return false
	}
	for i, account := range accounts {
		if expectedAccounts[i] != account {
			tc.t.Errorf("%s #%d: "+
				"account mismatch -- got %d, "+
				"want %d", prefix, i, account, expectedAccounts[i])
		}
	}
	return true
}

// testForEachAccountAddress tests that iterating through the given
// account addresses using the manager API works as expected.
func testForEachAccountAddress(tc *testContext) bool {
	prefix := testNamePrefix(tc) + " testForEachAccountAddress"
	// Make a map of expected addresses
	expectedAddrMap := make(map[string]*expectedAddr, len(expectedAddrs))
	for i := 0; i < len(expectedAddrs); i++ {
		expectedAddrMap[expectedAddrs[i].address] = &expectedAddrs[i]
	}

	var addrs []ManagedAddress
	err := walletdb.View(tc.db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		return tc.manager.ForEachAccountAddress(ns, tc.account,
			func(maddr ManagedAddress) error {
				addrs = append(addrs, maddr)
				return nil
			})
	})
	if err != nil {
		tc.t.Errorf("%s: unexpected error: %v", prefix, err)
		return false
	}

	for i := 0; i < len(addrs); i++ {
		prefix := fmt.Sprintf("%s: #%d", prefix, i)

		gotAddr := addrs[i]
		wantAddr := expectedAddrMap[gotAddr.Address().String()]
		if !testAddress(tc, prefix, gotAddr, wantAddr) {
			return false
		}
		delete(expectedAddrMap, gotAddr.Address().String())
	}

	if len(expectedAddrMap) != 0 {
		tc.t.Errorf("%s: unexpected addresses -- got %d, want %d", prefix,
			len(expectedAddrMap), 0)
		return false
	}

	return true
}

// testManagerAPI tests the functions provided by the Manager API as well as
// the ManagedAddress, ManagedPubKeyAddress, and ManagedScriptAddress
// interfaces.
func testManagerAPI(tc *testContext) {
	testLocking(tc)
	testExternalAddresses(tc)
	testInternalAddresses(tc)
	testImportPrivateKey(tc)
	testImportScript(tc)
	testMarkUsed(tc)
	testChangePassphrase(tc)

	// Reset default account
	tc.account = 0
	testNewAccount(tc)
	testLookupAccount(tc)
	testForEachAccount(tc)
	testForEachAccountAddress(tc)

	// Rename account 1 "acct-create"
	tc.account = 1
	testRenameAccount(tc)
}

// testWatchingOnly tests various facets of a watching-only address
// manager such as running the full set of API tests against a newly converted
// copy as well as when it is opened from an existing namespace.
func testWatchingOnly(tc *testContext) bool {
	// Make a copy of the current database so the copy can be converted to
	// watching only.
	woMgrName := "mgrtestwo.bin"
	_ = os.Remove(woMgrName)
	fi, err := os.OpenFile(woMgrName, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		tc.t.Errorf("%v", err)
		return false
	}
	if err := tc.db.Copy(fi); err != nil {
		fi.Close()
		tc.t.Errorf("%v", err)
		return false
	}
	fi.Close()
	defer os.Remove(woMgrName)

	// Open the new database copy and get the address manager namespace.
	db, err := walletdb.Open("bdb", woMgrName, true)
	if err != nil {
		tc.t.Errorf("openDbNamespace: unexpected error: %v", err)
		return false
	}
	defer db.Close()

	// Open the manager using the namespace and convert it to watching-only.
	var mgr *Manager
	err = walletdb.View(db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		var err error
		mgr, err = Open(ns, pubPassphrase, &chaincfg.MainNetParams)
		return err
	})
	if err != nil {
		tc.t.Errorf("%v", err)
		return false
	}
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		return mgr.ConvertToWatchingOnly(ns)
	})
	if err != nil {
		tc.t.Errorf("%v", err)
		return false
	}

	// Run all of the manager API tests against the converted manager and
	// close it. We'll also retrieve the default scope (BIP0044) from the
	// manager in order to use.
	scopedMgr, err := mgr.FetchScopedKeyManager(KeyScopeBIP0044)
	if err != nil {
		tc.t.Errorf("unable to fetch bip 44 scope %v", err)
		return false
	}
	testManagerAPI(&testContext{
		t:            tc.t,
		db:           db,
		rootManager:  mgr,
		manager:      scopedMgr,
		account:      0,
		create:       false,
		watchingOnly: true,
	})
	mgr.Close()

	// Open the watching-only manager and run all the tests again.
	err = walletdb.View(db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		var err error
		mgr, err = Open(ns, pubPassphrase, &chaincfg.MainNetParams)
		return err
	})
	if err != nil {
		tc.t.Errorf("Open Watching-Only: unexpected error: %v", err)
		return false
	}
	defer mgr.Close()

	scopedMgr, err = mgr.FetchScopedKeyManager(KeyScopeBIP0044)
	if err != nil {
		tc.t.Errorf("unable to fetch bip 44 scope %v", err)
		return false
	}

	testManagerAPI(&testContext{
		t:            tc.t,
		db:           db,
		rootManager:  mgr,
		manager:      scopedMgr,
		account:      0,
		create:       false,
		watchingOnly: true,
	})

	return true
}

// testSync tests various facets of setting the manager sync state.
func testSync(tc *testContext) bool {
	// Ensure syncing the manager to nil results in the synced to state
	// being the earliest block (genesis block in this case).
	err := walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		return tc.rootManager.SetSyncedTo(ns, nil)
	})
	if err != nil {
		tc.t.Errorf("SetSyncedTo unexpected err on nil: %v", err)
		return false
	}
	blockStamp := BlockStamp{
		Height: 0,
		Hash:   *chaincfg.MainNetParams.GenesisHash,
	}
	gotBlockStamp := tc.rootManager.SyncedTo()
	if gotBlockStamp != blockStamp {
		tc.t.Errorf("SyncedTo unexpected block stamp on nil -- "+
			"got %v, want %v", gotBlockStamp, blockStamp)
		return false
	}

	// If we update to a new more recent block time stamp, then upon
	// retrieval it should be returned as the best known state.
	latestHash, err := chainhash.NewHash(seed)
	if err != nil {
		tc.t.Errorf("%v", err)
		return false
	}
	blockStamp = BlockStamp{
		Height:    1,
		Hash:      *latestHash,
		Timestamp: time.Unix(1234, 0),
	}
	err = walletdb.Update(tc.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		return tc.rootManager.SetSyncedTo(ns, &blockStamp)
	})
	if err != nil {
		tc.t.Errorf("SetSyncedTo unexpected err on nil: %v", err)
		return false
	}
	gotBlockStamp = tc.rootManager.SyncedTo()
	if gotBlockStamp != blockStamp {
		tc.t.Errorf("SyncedTo unexpected block stamp on nil -- "+
			"got %v, want %v", gotBlockStamp, blockStamp)
		return false
	}

	return true
}

// TestManager performs a full suite of tests against the address manager API.
// It makes use of a test context because the address manager is persistent and
// much of the testing involves having specific state.
func TestManager(t *testing.T) {
	t.Parallel()

	teardown, db := emptyDB(t)
	defer teardown()

	// Open manager that does not exist to ensure the expected error is
	// returned.
	err := walletdb.View(db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		_, err := Open(ns, pubPassphrase, &chaincfg.MainNetParams)
		return err
	})
	if !checkManagerError(t, "Open non-existant", err, ErrNoExist) {
		return
	}

	// Create a new manager.
	var mgr *Manager
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns, err := tx.CreateTopLevelBucket(waddrmgrNamespaceKey)
		if err != nil {
			return err
		}
		err = Create(
			ns, seed, pubPassphrase, privPassphrase,
			&chaincfg.MainNetParams, fastScrypt, time.Time{},
		)
		if err != nil {
			return err
		}
		mgr, err = Open(ns, pubPassphrase, &chaincfg.MainNetParams)
		return err
	})
	if err != nil {
		t.Errorf("Create/Open: unexpected error: %v", err)
		return
	}

	// NOTE: Not using deferred close here since part of the tests is
	// explicitly closing the manager and then opening the existing one.

	// Attempt to create the manager again to ensure the expected error is
	// returned.
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		return Create(
			ns, seed, pubPassphrase, privPassphrase,
			&chaincfg.MainNetParams, fastScrypt, time.Time{},
		)
	})
	if !checkManagerError(t, "Create existing", err, ErrAlreadyExists) {
		mgr.Close()
		return
	}

	// Run all of the manager API tests in create mode and close the
	// manager after they've completed
	scopedMgr, err := mgr.FetchScopedKeyManager(KeyScopeBIP0044)
	if err != nil {
		t.Fatalf("unable to fetch default scope: %v", err)
	}
	testManagerAPI(&testContext{
		t:            t,
		db:           db,
		manager:      scopedMgr,
		rootManager:  mgr,
		account:      0,
		create:       true,
		watchingOnly: false,
	})
	mgr.Close()

	// Open the manager and run all the tests again in open mode which
	// avoids reinserting new addresses like the create mode tests do.
	err = walletdb.View(db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		var err error
		mgr, err = Open(ns, pubPassphrase, &chaincfg.MainNetParams)
		return err
	})
	if err != nil {
		t.Errorf("Open: unexpected error: %v", err)
		return
	}
	defer mgr.Close()

	scopedMgr, err = mgr.FetchScopedKeyManager(KeyScopeBIP0044)
	if err != nil {
		t.Fatalf("unable to fetch default scope: %v", err)
	}
	tc := &testContext{
		t:            t,
		db:           db,
		manager:      scopedMgr,
		rootManager:  mgr,
		account:      0,
		create:       false,
		watchingOnly: false,
	}
	testManagerAPI(tc)

	// Now that the address manager has been tested in both the newly
	// created and opened modes, test a watching-only version.
	testWatchingOnly(tc)

	// Ensure that the manager sync state functionality works as expected.
	testSync(tc)

	// Unlock the manager so it can be closed with it unlocked to ensure
	// it works without issue.
	err = walletdb.View(db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		return mgr.Unlock(ns, privPassphrase)
	})
	if err != nil {
		t.Errorf("Unlock: unexpected error: %v", err)
	}
}

// TestManagerIncorrectVersion ensures that that the manager cannot be accessed
// if its version does not match the latest version.
func TestManagerHigherVersion(t *testing.T) {
	t.Parallel()

	teardown, db, _ := setupManager(t)
	defer teardown()

	// We'll update our manager's version to be one higher than the latest.
	latestVersion := getLatestVersion()
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		if ns == nil {
			return errors.New("top-level namespace does not exist")
		}
		return putManagerVersion(ns, latestVersion+1)
	})
	if err != nil {
		t.Fatalf("unable to update manager version %v", err)
	}

	// Then, upon attempting to open it without performing an upgrade, we
	// should expect to see the error ErrUpgrade.
	err = walletdb.View(db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		_, err := Open(ns, pubPassphrase, &chaincfg.MainNetParams)
		return err
	})
	if !checkManagerError(t, "Upgrade needed", err, ErrUpgrade) {
		t.Fatalf("expected error ErrUpgrade, got %v", err)
	}

	// We'll also update it so that it is one lower than the latest.
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		if ns == nil {
			return errors.New("top-level namespace does not exist")
		}
		return putManagerVersion(ns, latestVersion-1)
	})
	if err != nil {
		t.Fatalf("unable to update manager version %v", err)
	}

	// Finally, upon attempting to open it without performing an upgrade to
	// the latest version, we should also expect to see the error
	// ErrUpgrade.
	err = walletdb.View(db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		_, err := Open(ns, pubPassphrase, &chaincfg.MainNetParams)
		return err
	})
	if !checkManagerError(t, "Upgrade needed", err, ErrUpgrade) {
		t.Fatalf("expected error ErrUpgrade, got %v", err)
	}
}

// TestEncryptDecryptErrors ensures that errors which occur while encrypting and
// decrypting data return the expected errors.
func TestEncryptDecryptErrors(t *testing.T) {
	t.Parallel()

	teardown, db, mgr := setupManager(t)
	defer teardown()

	invalidKeyType := CryptoKeyType(0xff)
	if _, err := mgr.Encrypt(invalidKeyType, []byte{}); err == nil {
		t.Fatalf("Encrypt accepted an invalid key type!")
	}

	if _, err := mgr.Decrypt(invalidKeyType, []byte{}); err == nil {
		t.Fatalf("Encrypt accepted an invalid key type!")
	}

	if !mgr.IsLocked() {
		t.Fatal("Manager should be locked at this point.")
	}

	var err error
	// Now the mgr is locked and encrypting/decrypting with private
	// keys should fail.
	_, err = mgr.Encrypt(CKTPrivate, []byte{})
	checkManagerError(t, "encryption with private key fails when manager is locked",
		err, ErrLocked)

	_, err = mgr.Decrypt(CKTPrivate, []byte{})
	checkManagerError(t, "decryption with private key fails when manager is locked",
		err, ErrLocked)

	// Unlock the manager for these tests
	err = walletdb.View(db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		return mgr.Unlock(ns, privPassphrase)
	})
	if err != nil {
		t.Fatal("Attempted to unlock the manager, but failed:", err)
	}

	// Make sure to cover the ErrCrypto error path in Encrypt and Decrypt.
	// We'll use a mock private key that will fail upon running these
	// methods.
	mgr.cryptoKeyPriv = &failingCryptoKey{}

	_, err = mgr.Encrypt(CKTPrivate, []byte{})
	checkManagerError(t, "failed encryption", err, ErrCrypto)

	_, err = mgr.Decrypt(CKTPrivate, []byte{})
	checkManagerError(t, "failed decryption", err, ErrCrypto)
}

// TestEncryptDecrypt ensures that encrypting and decrypting data with the
// the various crypto key types works as expected.
func TestEncryptDecrypt(t *testing.T) {
	t.Parallel()

	teardown, db, mgr := setupManager(t)
	defer teardown()

	plainText := []byte("this is a plaintext")

	// Make sure address manager is unlocked
	err := walletdb.View(db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		return mgr.Unlock(ns, privPassphrase)
	})
	if err != nil {
		t.Fatal("Attempted to unlock the manager, but failed:", err)
	}

	keyTypes := []CryptoKeyType{
		CKTPublic,
		CKTPrivate,
		CKTScript,
	}

	for _, keyType := range keyTypes {
		cipherText, err := mgr.Encrypt(keyType, plainText)
		if err != nil {
			t.Fatalf("Failed to encrypt plaintext: %v", err)
		}

		decryptedCipherText, err := mgr.Decrypt(keyType, cipherText)
		if err != nil {
			t.Fatalf("Failed to decrypt plaintext: %v", err)
		}

		if !reflect.DeepEqual(decryptedCipherText, plainText) {
			t.Fatal("Got:", decryptedCipherText, ", want:", plainText)
		}
	}
}

// TestScopedKeyManagerManagement tests that callers are able to properly
// create, retrieve, and utilize new scoped managers outside the set of default
// created scopes.
func TestScopedKeyManagerManagement(t *testing.T) {
	t.Parallel()

	teardown, db := emptyDB(t)
	defer teardown()

	// We'll start the test by creating a new root manager that will be
	// used for the duration of the test.
	var mgr *Manager
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns, err := tx.CreateTopLevelBucket(waddrmgrNamespaceKey)
		if err != nil {
			return err
		}
		err = Create(
			ns, seed, pubPassphrase, privPassphrase,
			&chaincfg.MainNetParams, fastScrypt, time.Time{},
		)
		if err != nil {
			return err
		}

		mgr, err = Open(ns, pubPassphrase, &chaincfg.MainNetParams)
		if err != nil {
			return err
		}

		return mgr.Unlock(ns, privPassphrase)
	})
	if err != nil {
		t.Fatalf("create/open: unexpected error: %v", err)
	}

	// All the default scopes should have been created and loaded into
	// memory upon initial opening.
	for _, scope := range DefaultKeyScopes {
		_, err := mgr.FetchScopedKeyManager(scope)
		if err != nil {
			t.Fatalf("unable to fetch scope %v: %v", scope, err)
		}
	}

	// Next, ensure that if we create an internal and external addrs for
	// each of the default scope types, then they're derived according to
	// their schema.
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)

		for _, scope := range DefaultKeyScopes {
			sMgr, err := mgr.FetchScopedKeyManager(scope)
			if err != nil {
				t.Fatalf("unable to fetch scope %v: %v", scope, err)
			}

			externalAddr, err := sMgr.NextExternalAddresses(
				ns, DefaultAccountNum, 1,
			)
			if err != nil {
				t.Fatalf("unable to derive external addr: %v", err)
			}

			// The external address should match the prescribed
			// addr schema for this scoped key manager.
			if externalAddr[0].AddrType() != ScopeAddrMap[scope].ExternalAddrType {
				t.Fatalf("addr type mismatch: expected %v, got %v",
					externalAddr[0].AddrType(),
					ScopeAddrMap[scope].ExternalAddrType)
			}

			internalAddr, err := sMgr.NextInternalAddresses(
				ns, DefaultAccountNum, 1,
			)
			if err != nil {
				t.Fatalf("unable to derive internal addr: %v", err)
			}

			// Similarly, the internal address should match the
			// prescribed addr schema for this scoped key manager.
			if internalAddr[0].AddrType() != ScopeAddrMap[scope].InternalAddrType {
				t.Fatalf("addr type mismatch: expected %v, got %v",
					internalAddr[0].AddrType(),
					ScopeAddrMap[scope].InternalAddrType)
			}
		}

		return err
	})
	if err != nil {
		t.Fatalf("unable to read db: %v", err)
	}

	// Now that the manager is open, we'll create a "test" scope that we'll
	// be utilizing for the remainder of the test.
	testScope := KeyScope{
		Purpose: 99,
		Coin:    0,
	}
	addrSchema := ScopeAddrSchema{
		ExternalAddrType: PubKeyHash,
		InternalAddrType: RawPubKey,
	}
	var scopedMgr *ScopedKeyManager
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)

		scopedMgr, err = mgr.NewScopedKeyManager(ns, testScope, addrSchema)
		return err
	})
	if err != nil {
		t.Fatalf("unable to read db: %v", err)
	}

	// The manager was just created, we should be able to look it up within
	// the root manager.
	if _, err := mgr.FetchScopedKeyManager(testScope); err != nil {
		t.Fatalf("attempt to read created mgr failed: %v", err)
	}

	var externalAddr, internalAddr []ManagedAddress
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)

		// We'll now create a new external address to ensure we
		// retrieve the proper type.
		externalAddr, err = scopedMgr.NextExternalAddresses(
			ns, DefaultAccountNum, 1,
		)
		if err != nil {
			t.Fatalf("unable to derive external addr: %v", err)
		}

		internalAddr, err = scopedMgr.NextInternalAddresses(
			ns, DefaultAccountNum, 1,
		)
		if err != nil {
			t.Fatalf("unable to derive internal addr: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("open: unexpected error: %v", err)
	}

	// Ensure that the type of the address matches as expected.
	if externalAddr[0].AddrType() != PubKeyHash {
		t.Fatalf("addr type mismatch: expected %v, got %v",
			PubKeyHash, externalAddr[0].AddrType())
	}
	_, ok := externalAddr[0].Address().(*bchutil.AddressPubKeyHash)
	if !ok {
		t.Fatalf("wrong type: %T", externalAddr[0].Address())
	}

	// We'll also create an internal address and ensure that the types
	// match up properly.
	if internalAddr[0].AddrType() != RawPubKey {
		t.Fatalf("addr type mismatch: expected %v, got %v",
			RawPubKey, internalAddr[0].AddrType())
	}
	_, ok = internalAddr[0].Address().(*bchutil.AddressPubKey)
	if !ok {
		t.Fatalf("wrong type: %T", externalAddr[0].Address())
	}

	// We'll now simulate a restart by closing, then restarting the
	// manager.
	mgr.Close()
	err = walletdb.View(db, func(tx walletdb.ReadTx) error {
		ns := tx.ReadBucket(waddrmgrNamespaceKey)
		var err error
		mgr, err = Open(ns, pubPassphrase, &chaincfg.MainNetParams)
		if err != nil {
			return err
		}

		return mgr.Unlock(ns, privPassphrase)
	})
	if err != nil {
		t.Fatalf("open: unexpected error: %v", err)
	}
	defer mgr.Close()

	// We should be able to retrieve the new scoped manager that we just
	// created.
	scopedMgr, err = mgr.FetchScopedKeyManager(testScope)
	if err != nil {
		t.Fatalf("attempt to read created mgr failed: %v", err)
	}

	// If we fetch the last generated external address, it should map
	// exactly to the address that we just generated.
	var lastAddr ManagedAddress
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)

		lastAddr, err = scopedMgr.LastExternalAddress(
			ns, DefaultAccountNum,
		)
		return err
	})
	if err != nil {
		t.Fatalf("open: unexpected error: %v", err)
	}
	if !bytes.Equal(lastAddr.AddrHash(), externalAddr[0].AddrHash()) {
		t.Fatalf("mismatch addr hashes: expected %x, got %x",
			externalAddr[0].AddrHash(), lastAddr.AddrHash())
	}

	// After the restart, all the default scopes should be been re-loaded.
	for _, scope := range DefaultKeyScopes {
		_, err := mgr.FetchScopedKeyManager(scope)
		if err != nil {
			t.Fatalf("unable to fetch scope %v: %v", scope, err)
		}
	}

	// Finally, if we attempt to query the root manager for this last
	// address, it should be able to locate the private key, etc.
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)

		_, err := mgr.Address(ns, lastAddr.Address())
		if err != nil {
			return fmt.Errorf("unable to find addr: %v", err)
		}

		err = mgr.MarkUsed(ns, lastAddr.Address())
		if err != nil {
			return fmt.Errorf("unable to mark addr as "+
				"used: %v", err)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("unable to find addr: %v", err)
	}
}

// TestRootHDKeyNeutering tests that callers are unable to create new scoped
// managers once the root HD key has been deleted from the database.
func TestRootHDKeyNeutering(t *testing.T) {
	t.Parallel()

	teardown, db := emptyDB(t)
	defer teardown()

	// We'll start the test by creating a new root manager that will be
	// used for the duration of the test.
	var mgr *Manager
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns, err := tx.CreateTopLevelBucket(waddrmgrNamespaceKey)
		if err != nil {
			return err
		}
		err = Create(
			ns, seed, pubPassphrase, privPassphrase,
			&chaincfg.MainNetParams, fastScrypt, time.Time{},
		)
		if err != nil {
			return err
		}

		mgr, err = Open(ns, pubPassphrase, &chaincfg.MainNetParams)
		if err != nil {
			return err
		}

		return mgr.Unlock(ns, privPassphrase)
	})
	if err != nil {
		t.Fatalf("create/open: unexpected error: %v", err)
	}
	defer mgr.Close()

	// With the root manager open, we'll now create a new scoped manager
	// for usage within this test.
	testScope := KeyScope{
		Purpose: 99,
		Coin:    0,
	}
	addrSchema := ScopeAddrSchema{
		ExternalAddrType: PubKeyHash,
		InternalAddrType: RawPubKey,
	}
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)

		_, err := mgr.NewScopedKeyManager(ns, testScope, addrSchema)
		return err
	})
	if err != nil {
		t.Fatalf("unable to read db: %v", err)
	}

	// With the manager created, we'll now neuter the root HD private key.
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)

		return mgr.NeuterRootKey(ns)
	})
	if err != nil {
		t.Fatalf("unable to read db: %v", err)
	}

	// If we try to create *another* scope, this should fail, as the root
	// key is no longer in the database.
	testScope = KeyScope{
		Purpose: 100,
		Coin:    0,
	}
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)

		_, err := mgr.NewScopedKeyManager(ns, testScope, addrSchema)
		return err
	})
	if err == nil {
		t.Fatalf("new scoped manager creation should have failed")
	}
}

// TestNewRawAccount tests that callers are able to properly create, and use
// raw accounts created with only an account number, and not a string which is
// eventually mapped to an account number.
func TestNewRawAccount(t *testing.T) {
	//t.Parallel()

	teardown, db := emptyDB(t)
	defer teardown()

	// We'll start the test by creating a new root manager that will be
	// used for the duration of the test.
	var mgr *Manager
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns, err := tx.CreateTopLevelBucket(waddrmgrNamespaceKey)
		if err != nil {
			return err
		}
		err = Create(
			ns, seed, pubPassphrase, privPassphrase,
			&chaincfg.MainNetParams, fastScrypt, time.Time{},
		)
		if err != nil {
			return err
		}

		mgr, err = Open(ns, pubPassphrase, &chaincfg.MainNetParams)
		if err != nil {
			return err
		}

		return mgr.Unlock(ns, privPassphrase)
	})
	if err != nil {
		t.Fatalf("create/open: unexpected error: %v", err)
	}
	defer mgr.Close()

	// Now that we have the manager created, we'll fetch one of the default
	// scopes for usage within this test.
	scopedMgr, err := mgr.FetchScopedKeyManager(KeyScopeBIP0044)
	if err != nil {
		t.Fatalf("unable to fetch scope %v: %v", KeyScopeBIP0044, err)
	}

	// With the scoped manager retrieved, we'll attempt to create a new raw
	// account by number.
	const accountNum = 1000
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		return scopedMgr.NewRawAccount(ns, accountNum)
	})
	if err != nil {
		t.Fatalf("unable to create new account: %v", err)
	}

	// With the account created, we should be able to derive new addresses
	// from the account.
	var accountAddrNext ManagedAddress
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)

		addrs, err := scopedMgr.NextExternalAddresses(
			ns, accountNum, 1,
		)
		if err != nil {
			return err
		}

		accountAddrNext = addrs[0]
		return nil
	})
	if err != nil {
		t.Fatalf("unable to create addr: %v", err)
	}

	// Additionally, we should be able to manually derive specific target
	// keys.
	var accountTargetAddr ManagedAddress
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)

		keyPath := DerivationPath{
			Account: accountNum,
			Branch:  0,
			Index:   0,
		}
		accountTargetAddr, err = scopedMgr.DeriveFromKeyPath(
			ns, keyPath,
		)
		return err
	})
	if err != nil {
		t.Fatalf("unable to derive addr: %v", err)
	}

	// The two keys we just derived should match up perfectly.
	if accountAddrNext.AddrType() != accountTargetAddr.AddrType() {
		t.Fatalf("wrong addr type: %v vs %v",
			accountAddrNext.AddrType(), accountTargetAddr.AddrType())
	}
	if !bytes.Equal(accountAddrNext.AddrHash(), accountTargetAddr.AddrHash()) {
		t.Fatalf("wrong pubkey hash: %x vs %x", accountAddrNext.AddrHash(),
			accountTargetAddr.AddrHash())
	}
}
