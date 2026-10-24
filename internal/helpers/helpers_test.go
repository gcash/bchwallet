package helpers

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tyler-smith/go-bip39"
)

// TestSeedFromMnemonic checks that SeedFromMnemonic rejects invalid mnemonics
// and derives the same seed as the canonical, single-spaced mnemonic.
func TestSeedFromMnemonic(t *testing.T) {
	valid12 := strings.Repeat("abandon ", 11) + "about"
	valid24 := strings.Repeat("abandon ", 23) + "art"

	tests := []struct {
		name     string
		mnemonic string

		// canonical is the mnemonic the seed must be derived from, or
		// empty if the mnemonic must be rejected.
		canonical string
	}{
		{"valid 12 words", valid12, valid12},
		{"valid 24 words", valid24, valid24},
		{
			"extra whitespace",
			"  " + strings.ReplaceAll(valid12, " ", " \t ") + "\n",
			valid12,
		},
		{"bad checksum", strings.Repeat("abandon ", 11) + "abandon", ""},
		{"unknown word", strings.Repeat("abandon ", 11) + "notaword", ""},
		{"empty", "", ""},
	}

	for _, test := range tests {
		seed, err := SeedFromMnemonic(test.mnemonic)
		if test.canonical == "" {
			if err != ErrInvalidMnemonic {
				t.Errorf("%s: got error %v, want %v", test.name,
					err, ErrInvalidMnemonic)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error: %v", test.name, err)
			continue
		}
		want := bip39.NewSeed(test.canonical, "")
		if !bytes.Equal(seed, want) {
			t.Errorf("%s: seed doesn't match the one derived from "+
				"the canonical mnemonic", test.name)
		}
	}
}
