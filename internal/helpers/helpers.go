// Copyright (c) 2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

// Package helpers provides convenience functions to simplify wallet code.  This
// package is intended for internal wallet use only.
package helpers

import (
	"errors"
	"strings"

	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/tyler-smith/go-bip39"
)

// ErrInvalidMnemonic is returned by SeedFromMnemonic when the mnemonic isn't a
// valid BIP39 mnemonic.
var ErrInvalidMnemonic = errors.New("invalid BIP39 mnemonic seed")

// SumOutputValues sums up the list of TxOuts and returns an Amount.
func SumOutputValues(outputs []*wire.TxOut) (totalOutput bchutil.Amount) {
	for _, txOut := range outputs {
		totalOutput += bchutil.Amount(txOut.Value)
	}
	return totalOutput
}

// SumOutputSerializeSizes sums up the serialized size of the supplied outputs.
func SumOutputSerializeSizes(outputs []*wire.TxOut) (serializeSize int) {
	for _, txOut := range outputs {
		serializeSize += txOut.SerializeSize()
	}
	return serializeSize
}

// SeedFromMnemonic checks that mnemonic is a valid BIP39 mnemonic, with words
// from the wordlist and a matching checksum, and returns the seed derived from
// it.  Words are first separated by single spaces, the form BIP39 defines, so
// stray whitespace doesn't produce a different seed.
func SeedFromMnemonic(mnemonic string) ([]byte, error) {
	mnemonic = strings.Join(strings.Fields(mnemonic), " ")
	if !bip39.IsMnemonicValid(mnemonic) {
		return nil, ErrInvalidMnemonic
	}
	return bip39.NewSeed(mnemonic, ""), nil
}
