// Copyright (c) 2014 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package bdb_test

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gcash/bchwallet/walletdb"
	_ "github.com/gcash/bchwallet/walletdb/bdb"
)

// dbType is the database type name for this driver.
const dbType = "bdb"

// TestCreateOpenFail ensures that errors related to creating and opening a
// database are handled properly.
func TestCreateOpenFail(t *testing.T) {
	// Ensure that attempting to open a database that doesn't exist returns
	// the expected error.
	wantErr := walletdb.ErrDbDoesNotExist
	if _, err := walletdb.Open(dbType, "noexist.db", true); err != wantErr {
		t.Errorf("Open: did not receive expected error - got %v, "+
			"want %v", err, wantErr)
		return
	}

	// Ensure that attempting to open a database with the wrong number of
	// parameters returns the expected error.
	wantErr = fmt.Errorf("invalid arguments to %s.Open -- expected "+
		"database path and no-freelist-sync option", dbType)
	if _, err := walletdb.Open(dbType, 1, 2, 3); err.Error() != wantErr.Error() {
		t.Errorf("Open: did not receive expected error - got %v, "+
			"want %v", err, wantErr)
		return
	}

	// Ensure that attempting to open a database with an invalid type for
	// the first parameter returns the expected error.
	wantErr = fmt.Errorf("first argument to %s.Open is invalid -- "+
		"expected database path string", dbType)
	if _, err := walletdb.Open(dbType, 1, true); err.Error() != wantErr.Error() {
		t.Errorf("Open: did not receive expected error - got %v, "+
			"want %v", err, wantErr)
		return
	}

	// Ensure that attempting to create a database with the wrong number of
	// parameters returns the expected error.
	wantErr = fmt.Errorf("invalid arguments to %s.Create -- expected "+
		"database path and no-freelist-sync option", dbType)
	if _, err := walletdb.Create(dbType, 1, 2, 3); err.Error() != wantErr.Error() {
		t.Errorf("Create: did not receive expected error - got %v, "+
			"want %v", err, wantErr)
		return
	}

	// Ensure that attempting to open a database with an invalid type for
	// the first parameter returns the expected error.
	wantErr = fmt.Errorf("first argument to %s.Create is invalid -- "+
		"expected database path string", dbType)
	if _, err := walletdb.Create(dbType, 1, true); err.Error() != wantErr.Error() {
		t.Errorf("Create: did not receive expected error - got %v, "+
			"want %v", err, wantErr)
		return
	}

	// Ensure operations against a closed database return the expected
	// error.
	tempDir, err := ioutil.TempDir("", "createfail")
	if err != nil {
		t.Errorf("unable to create temp dir: %v", err)
		return
	}
	defer os.Remove(tempDir)

	dbPath := filepath.Join(tempDir, "db")
	db, err := walletdb.Create(dbType, dbPath, true)
	if err != nil {
		t.Errorf("Create: unexpected error: %v", err)
		return
	}
	db.Close()

	wantErr = walletdb.ErrDbNotOpen
	if _, err := db.BeginReadTx(); err != wantErr {
		t.Errorf("Namespace: did not receive expected error - got %v, "+
			"want %v", err, wantErr)
		return
	}
}

// TestPersistence ensures that values stored are still valid after closing and
// reopening the database.
func TestPersistence(t *testing.T) {
	// Create a new database to run tests against.
	tempDir, err := ioutil.TempDir("", "persistencetest")
	if err != nil {
		t.Errorf("unable to create temp dir: %v", err)
		return
	}
	defer os.Remove(tempDir)

	dbPath := filepath.Join(tempDir, "db")
	db, err := walletdb.Create(dbType, dbPath, true)
	if err != nil {
		t.Errorf("Failed to create test database (%s) %v", dbType, err)
		return
	}
	defer db.Close()

	// Create a namespace and put some values into it so they can be tested
	// for existence on re-open.
	storeValues := map[string]string{
		"ns1key1": "foo1",
		"ns1key2": "foo2",
		"ns1key3": "foo3",
	}
	ns1Key := []byte("ns1")
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns1, err := tx.CreateTopLevelBucket(ns1Key)
		if err != nil {
			return err
		}

		for k, v := range storeValues {
			if err := ns1.Put([]byte(k), []byte(v)); err != nil {
				return fmt.Errorf("Put: unexpected error: %v", err)
			}
		}

		return nil
	})
	if err != nil {
		t.Errorf("ns1 Update: unexpected error: %v", err)
		return
	}

	// Close and reopen the database to ensure the values persist.
	db.Close()
	db, err = walletdb.Open(dbType, dbPath, true)
	if err != nil {
		t.Errorf("Failed to open test database (%s) %v", dbType, err)
		return
	}
	defer db.Close()

	// Ensure the values previously stored in the 3rd namespace still exist
	// and are correct.
	err = walletdb.View(db, func(tx walletdb.ReadTx) error {
		ns1 := tx.ReadBucket(ns1Key)
		if ns1 == nil {
			return fmt.Errorf("ReadTx.ReadBucket: unexpected nil root bucket")
		}

		for k, v := range storeValues {
			gotVal := ns1.Get([]byte(k))
			if !reflect.DeepEqual(gotVal, []byte(v)) {
				return fmt.Errorf("Get: key '%s' does not "+
					"match expected value - got %s, want %s",
					k, gotVal, v)
			}
		}

		return nil
	})
	if err != nil {
		t.Errorf("ns1 View: unexpected error: %v", err)
		return
	}
}
