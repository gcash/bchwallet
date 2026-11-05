package boot

import (
	"testing"

	chainbcmr "github.com/gcash/bchd/bcmr"
	"github.com/gcash/bchd/chaincfg/chainhash"
)

// TestAutoLoad ensures only a token in the table loads by itself, and that with
// no table none does.
func TestAutoLoad(t *testing.T) {
	inTable, notInTable := chainhash.Hash{1}, chainhash.Hash{2}
	hints, err := chainbcmr.ParseHints(chainbcmr.EncodeHints(10, []chainbcmr.Hint{{Category: inTable}}))
	if err != nil {
		t.Fatal(err)
	}
	load := autoLoad(hints)
	if !load(inTable) || load(notInTable) {
		t.Fatal("wrong answer from the table")
	}
	if autoLoad(nil)(inTable) {
		t.Fatal("a token loaded by itself with no table")
	}
}
