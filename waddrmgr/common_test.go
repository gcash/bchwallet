// Copyright (c) 2014 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package waddrmgr

import (
	"encoding/hex"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchwallet/walletdb"
	_ "github.com/gcash/bchwallet/walletdb/bdb"
)

var (
	// seed is the master seed used throughout the tests.
	seed = []byte{
		0x2a, 0x64, 0xdf, 0x08, 0x5e, 0xef, 0xed, 0xd8, 0xbf,
		0xdb, 0xb3, 0x31, 0x76, 0xb5, 0xba, 0x2e, 0x62, 0xe8,
		0xbe, 0x8b, 0x56, 0xc8, 0x83, 0x77, 0x95, 0x59, 0x8b,
		0xb6, 0xc4, 0x40, 0xc0, 0x64,
	}

	pubPassphrase   = []byte("_DJr{fL4H0O}*-0\n:V1izc)(6BomK")
	privPassphrase  = []byte("81lUHXnOMZ@?XXd7O9xyDIWIbXX-lj")
	pubPassphrase2  = []byte("-0NV4P~VSJBWbunw}%<Z]fuGpbN[ZI")
	privPassphrase2 = []byte("~{<]08%6!-?2s<$(8$8:f(5[4/!/{Y")

	// fastScrypt are parameters used throughout the tests to speed up the
	// scrypt operations.
	fastScrypt = &ScryptOptions{
		N: 16,
		R: 8,
		P: 1,
	}

	// waddrmgrNamespaceKey is the namespace key for the waddrmgr package.
	waddrmgrNamespaceKey = []byte("waddrmgrNamespace")

	// expectedAddrs is the list of all expected addresses generated from the
	// seed.
	expectedAddrs = []expectedAddr{
		{
			address:     "qq45nmxseaeqqctnum544n6pddnntd0c3ymme493qh",
			addressHash: hexToBytes("2b49ecd0cf72006173e6e95acf416b6735b5f889"),
			internal:    false,
			compressed:  true,
			imported:    false,
			pubKey:      hexToBytes("02d8f88468c5a2e8e1815faf555f59cbd1979e3dbdf823f80c271b6fb70d2d519b"),
			privKey:     hexToBytes("c27d6581b92785834b381fa697c4b0ffc4574b495743722e0acb7601b1b68b99"),
			privKeyWIF:  "L3jmpy54Pc7MLXTN2mL8Xas7BJziwKaUGmgnXXzgGbVRdiAniXZk",
			derivationInfo: DerivationPath{
				Account: 0,
				Branch:  0,
				Index:   0,
			},
		},
		{
			address:     "qrnvtxs4ggfc6xls3azu6xyfj4tu744n2ch2s8fnj4",
			addressHash: hexToBytes("e6c59a1542138d1bf08f45cd18899557cf56b356"),
			internal:    false,
			compressed:  true,
			imported:    false,
			pubKey:      hexToBytes("02b9c175b908624f8a8eaac227d0e8c77c0eec327b8c512ad1b8b7a4b5b676971f"),
			privKey:     hexToBytes("18f3b191019e83878a81557abebb2afda199e31d22e150d8bf4df4561671be6c"),
			privKeyWIF:  "Kx4DNid19W8sjNFN3uPqQE7UYnCqyEp7unCvdkf2LrVUFpnDtwpB",
			derivationInfo: DerivationPath{
				Account: 0,
				Branch:  0,
				Index:   1,
			},
		},
		{
			address:     "qqzkr6fh8xrfvkmy0ft6p9cca8q9qg2ulct2rf5w4a",
			addressHash: hexToBytes("0561e9373986965b647a57a09718e9c050215cfe"),
			internal:    false,
			compressed:  true,
			imported:    false,
			pubKey:      hexToBytes("0329faddf1254d490d6add49e2b08cf52b561038c72baec0edb3cfacff71ff1021"),
			privKey:     hexToBytes("ccb8f6305b73136b363644b647f6efc0fd27b6b7d9c11c7e560662ed38db7b34"),
			privKeyWIF:  "L45fWF6Yd736fDohuB97vwRRLdQQJr3ZGvbokk9ubiT7aNrg7tTn",
			derivationInfo: DerivationPath{
				Account: 0,
				Branch:  0,
				Index:   2,
			},
		},
		{
			address:     "qqd0j597qfvyeg3skurcemqvl5ud6ud5dqlahfdewn",
			addressHash: hexToBytes("1af950be02584ca230b7078cec0cfd38dd71b468"),
			internal:    false,
			compressed:  true,
			imported:    false,
			pubKey:      hexToBytes("03d738324e2f0ce42e46975d7f8c7117c1670e3d7912b0291aea452add99674774"),
			privKey:     hexToBytes("d6bc8ff768814fede2adcdb74826bd846924341b3862e3b6e31cdc084e992940"),
			privKeyWIF:  "L4R8XyxYQyPSpTwj8w96tM86a6j3QA9jbRPj3RA7DVTVWk71ndeP",
			derivationInfo: DerivationPath{
				Account: 0,
				Branch:  0,
				Index:   3,
			},
		},
		{
			address:     "qr2h3gn85ut5c6a87a4s4v3e0nst5rzu8szs53r690",
			addressHash: hexToBytes("d578a267a7174c6ba7f76b0ab2397ce0ba0c5c3c"),
			internal:    false,
			compressed:  true,
			imported:    false,
			pubKey:      hexToBytes("03a917acd5cd5b6f544b43f1921a35677e4d5320e5d2add2056039b4b44fdf905e"),
			privKey:     hexToBytes("8563ade061110e03aee50695ffc5cb1c06c8310bde0a3674257c853c966968c0"),
			privKeyWIF:  "L1h16Hunxomww4FrpyQP2iFmWNgG7U1u3awp6Vd3s2uGf7v5VU8c",
			derivationInfo: DerivationPath{
				Account: 0,
				Branch:  0,
				Index:   4,
			},
		},
		{
			address:     "qqh0jj4mnm500pwsslp7ertwu3n7jtgdpg9sf7za33",
			addressHash: hexToBytes("2ef94abb9ee8f785d087c3ec8d6ee467e92d0d0a"),
			internal:    true,
			compressed:  true,
			imported:    false,
			pubKey:      hexToBytes("020a1290b997c0a234a95213962e7edcb761c7360f0230f698a1a3e71c37047bb0"),
			privKey:     hexToBytes("fe4f855fcf059ec6ddf7b25f63b19aa49c771d1fcb9850b68ae3d65e20657a60"),
			privKeyWIF:  "L5k4HivqXvohxBMpuwD38iUgi6uewffwZny91ZNYfM39RXH2x3QR",
			derivationInfo: DerivationPath{
				Account: 0,
				Branch:  1,
				Index:   0,
			},
		},
		{
			address:     "qrfu3mzx3y04nxl74fxztyvtlv75d63nfs5mlnelyc",
			addressHash: hexToBytes("d3c8ec46891f599bfeaa4c25918bfb3d46ea334c"),
			internal:    true,
			compressed:  true,
			imported:    false,
			pubKey:      hexToBytes("03f79bbde32af42dde98195f011d95982602fcd0dab657fe4a1f49f9d5ada1e02d"),
			privKey:     hexToBytes("bfef521317c65b018ae7e6d7ecc3aa700d5d0f7ea84d567be9270382d0b5e3e6"),
			privKeyWIF:  "L3eomUajnTDM3Pc8GU47qqXUFuCjvpqY7NYN9mH3x1ZFjDgiY4BU",
			derivationInfo: DerivationPath{
				Account: 0,
				Branch:  1,
				Index:   1,
			},
		},
		{
			address:     "qqdq454qfl0rk2h7q6q9wkg7rpcu9zwyhq73wz9pk9",
			addressHash: hexToBytes("1a0ad2a04fde3b2afe068057591e1871c289c4b8"),
			internal:    true,
			compressed:  true,
			imported:    false,
			pubKey:      hexToBytes("023ded84afe4fe91b52b45c3deb26fd263f749cbc27747dc964dae9e0739cbc579"),
			privKey:     hexToBytes("f506dffd4494c24006df7a35f3291f7ca0297a1a431557a1339bfed6f48738ca"),
			privKeyWIF:  "L5S1bVQUPqQb1Su82fLoSpnGCjcPfdAQE1pJxWRopJSBdYNDHESv",
			derivationInfo: DerivationPath{
				Account: 0,
				Branch:  1,
				Index:   2,
			},
		},
		{
			address:     "qp5fkqjfcc5zv5s4l5w7v9pdt42ef6udcgmq7kp37p",
			addressHash: hexToBytes("689b0249c628265215fd1de6142d5d5594eb8dc2"),
			internal:    true,
			compressed:  true,
			imported:    false,
			pubKey:      hexToBytes("030f1e79f06824e10a259914ec310528bb2d5b8d6356341fe9dff55498591af6af"),
			privKey:     hexToBytes("b3629de8ef6a275b4ffae41aa2bbbc2952eb92282ea6402435abbb010ecc1fb8"),
			privKeyWIF:  "L3EQsGeEnyXmKaux54cG4DQeCSQDvGuvEuy3W2ss4geum7AtWaHw",
			derivationInfo: DerivationPath{
				Account: 0,
				Branch:  1,
				Index:   3,
			},
		},
		{
			address:     "qrq3mk9r2auh3qr6q3fla0k7u2v55c2y6sl5v0uuh3",
			addressHash: hexToBytes("c11dd8a3577978807a0453febedee2994a6144d4"),
			internal:    true,
			compressed:  true,
			imported:    false,
			pubKey:      hexToBytes("0317d7182e26b6ca3e0f3db531c474b9cab7a763a75eabff2e14ac92f62a793238"),
			privKey:     hexToBytes("ca747a7ef815ea0dbe68655272cecbfbd65f2a109019a9ed28e0d3dcaffe05c3"),
			privKeyWIF:  "L41Frac75RPbTELKzw1EGC2qCkdveiVumpmsyX4daAvyyCMxit1W",
			derivationInfo: DerivationPath{
				Account: 0,
				Branch:  1,
				Index:   4,
			},
		},
	}

	// expectedExternalAddrs is the list of expected external addresses
	// generated from the seed
	expectedExternalAddrs = expectedAddrs[:5]

	// expectedInternalAddrs is the list of expected internal addresses
	// generated from the seed
	expectedInternalAddrs = expectedAddrs[5:]
)

// checkManagerError ensures the passed error is a ManagerError with an error
// code that matches the passed  error code.
func checkManagerError(t *testing.T, testName string, gotErr error,
	wantErrCode ErrorCode) bool {

	merr, ok := gotErr.(ManagerError)
	if !ok {
		t.Errorf("%s: unexpected error type - got %T, want %T",
			testName, gotErr, ManagerError{})
		return false
	}
	if merr.ErrorCode != wantErrCode {
		t.Errorf("%s: unexpected error code - got %s (%s), want %s",
			testName, merr.ErrorCode, merr.Description, wantErrCode)
		return false
	}

	return true
}

// hexToBytes is a wrapper around hex.DecodeString that panics if there is an
// error.  It MUST only be used with hard coded values in the tests.
func hexToBytes(origHex string) []byte {
	buf, err := hex.DecodeString(origHex)
	if err != nil {
		panic(err)
	}
	return buf
}

func emptyDB(t *testing.T) (tearDownFunc func(), db walletdb.DB) {
	dirName, err := ioutil.TempDir("", "mgrtest")
	if err != nil {
		t.Fatalf("Failed to create db temp dir: %v", err)
	}
	dbPath := filepath.Join(dirName, "mgrtest.db")
	db, err = walletdb.Create("bdb", dbPath, true)
	if err != nil {
		_ = os.RemoveAll(dirName)
		t.Fatalf("createDbNamespace: unexpected error: %v", err)
	}
	tearDownFunc = func() {
		db.Close()
		_ = os.RemoveAll(dirName)
	}
	return
}

// setupManager creates a new address manager and returns a teardown function
// that should be invoked to ensure it is closed and removed upon completion.
func setupManager(t *testing.T) (tearDownFunc func(), db walletdb.DB, mgr *Manager) {
	// Create a new manager in a temp directory.
	dirName, err := ioutil.TempDir("", "mgrtest")
	if err != nil {
		t.Fatalf("Failed to create db temp dir: %v", err)
	}
	dbPath := filepath.Join(dirName, "mgrtest.db")
	db, err = walletdb.Create("bdb", dbPath, true)
	if err != nil {
		_ = os.RemoveAll(dirName)
		t.Fatalf("createDbNamespace: unexpected error: %v", err)
	}
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
		db.Close()
		_ = os.RemoveAll(dirName)
		t.Fatalf("Failed to create Manager: %v", err)
	}
	tearDownFunc = func() {
		mgr.Close()
		db.Close()
		_ = os.RemoveAll(dirName)
	}
	return tearDownFunc, db, mgr
}
