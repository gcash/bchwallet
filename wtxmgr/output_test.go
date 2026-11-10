// Copyright (c) 2026 The bchwallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wtxmgr

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/wire"
)

// manyPartsTx returns a transaction with inputs of scripts of several
// lengths, one long enough to need a three byte length, and outputs with and
// without tokens, as a transaction a CashFusion makes has many of both.
func manyPartsTx(t *testing.T) *wire.MsgTx {
	t.Helper()
	tx := wire.NewMsgTx(wire.TxVersion)
	for i, n := range []int{0, 107, 300, 1, 72} {
		tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{
			Hash:  chainhash.Hash{byte(i + 1)},
			Index: uint32(i),
		}, bytes.Repeat([]byte{byte(i)}, n)))
	}
	var category [32]byte
	category[0] = 0xca
	amount := uint64(1000)
	fungible, err := wire.NewTokenData(category, &amount, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	commitment := []byte{1, 2, 3}
	none := byte(wire.NONE)
	nft, err := wire.NewTokenData(category, nil, &commitment, &none)
	if err != nil {
		t.Fatal(err)
	}
	p2pkh := append(append([]byte{0x76, 0xa9, 0x14}, bytes.Repeat([]byte{7}, 20)...), 0x88, 0xac)
	tx.AddTxOut(wire.NewTxOut(10000, p2pkh, wire.TokenData{}))
	tx.AddTxOut(wire.NewTxOut(1000, p2pkh, *fungible))
	tx.AddTxOut(wire.NewTxOut(0, bytes.Repeat([]byte{0x6a}, 300), wire.TokenData{}))
	tx.AddTxOut(wire.NewTxOut(800, p2pkh, *nft))
	tx.AddTxOut(wire.NewTxOut(5000, p2pkh, wire.TokenData{}))
	return tx
}

// TestReadRawTxRecordOutput checks that reading a single output of a recorded
// transaction gives the output that decoding all of it does, at every index,
// and that an index past the outputs, or a record cut short, is an error.
func TestReadRawTxRecordOutput(t *testing.T) {
	tx := manyPartsTx(t)
	rec, err := NewTxRecordFromMsgTx(tx, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	v, err := valueTxRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range tx.TxOut {
		got, err := readRawTxRecordOutput(v, uint32(i))
		if err != nil {
			t.Fatalf("output %d: %v", i, err)
		}
		if got.Value != want.Value || !bytes.Equal(got.PkScript, want.PkScript) ||
			!reflect.DeepEqual(got.TokenData, want.TokenData) {

			t.Errorf("output %d: got %+v, want %+v", i, got, want)
		}
	}
	if _, err := readRawTxRecordOutput(v, uint32(len(tx.TxOut))); err == nil {
		t.Error("an index past the outputs gave an output")
	}
	// The last output ends 4 bytes before the record, which ends with the
	// lock time, so a cut 5 bytes from the end is into it.
	for _, cut := range []int{0, 7, 12, 50, len(v) - 5} {
		if _, err := readRawTxRecordOutput(v[:cut], uint32(len(tx.TxOut)-1)); err == nil {
			t.Errorf("a record cut to %d bytes gave an output", cut)
		}
	}
}
