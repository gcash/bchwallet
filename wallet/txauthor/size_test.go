// Copyright (c) 2026 The bchwallet developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package txauthor_test

import (
	"errors"
	"testing"

	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	. "github.com/gcash/bchwallet/wallet/txauthor"
	"github.com/gcash/bchwallet/wallet/txrules"
	"github.com/gcash/bchwallet/wallet/txsizes"
)

// TestTooLargeForOneTransaction checks that an amount that takes more inputs
// than fit in one standard transaction is refused before it's signed, with the
// most that one transaction can send, and that one that fits is built.
func TestTooLargeForOneTransaction(t *testing.T) {
	coins := func() []*wire.TxOut {
		v := make([]*wire.TxOut, 1000)
		for i := range v {
			v[i] = wire.NewTxOut(10000, make([]byte, txsizes.P2PKHOutputSize), wire.TokenData{})
		}
		return v
	}
	fee := txrules.DefaultRelayFeePerKb
	change := func() ([]byte, error) { return make([]byte, txsizes.P2PKHPkScriptSize), nil }

	// 1000 coins of 10,000 sat: 9,000,000 sat needs over 900 of them.
	_, err := NewUnsignedTransaction(p2pkhOutputs(9000000), fee, makeInputSource(coins()), change)
	var tooLarge TooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("an amount of 900 coins gave %v, want TooLargeError", err)
	}
	// As many coins as fit, less their fee.
	n := 0
	for txsizes.EstimateSerializeSize(n+1, p2pkhOutputs(0), true) <= txrules.MaxStandardTxSize {
		n++
	}
	want := bchutil.Amount(n*10000) - txrules.FeeForSerializeSize(fee,
		txsizes.EstimateSerializeSize(n, p2pkhOutputs(0), true))
	if tooLarge.Max != want {
		t.Fatalf("most in one transaction: %v, want %v", tooLarge.Max, want)
	}

	// What it says can be sent is built.
	tx, err := NewUnsignedTransaction(p2pkhOutputs(tooLarge.Max), fee, makeInputSource(coins()), change)
	if err != nil {
		t.Fatalf("the most it said fits: %v", err)
	}
	if size := txsizes.EstimateSerializeSize(len(tx.Tx.TxIn), tx.Tx.TxOut, false); size > txrules.MaxStandardTxSize {
		t.Fatalf("a transaction of %d bytes", size)
	}
}
