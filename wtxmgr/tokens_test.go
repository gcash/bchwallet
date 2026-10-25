package wtxmgr

import (
	"bytes"
	"testing"
	"time"

	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/wire"
)

// TestUnspentOutputsTokenData checks that unspent outputs keep the CashTokens
// they carry, so that the wallet can tell them apart and not spend them.
func TestUnspentOutputsTokenData(t *testing.T) {
	t.Parallel()

	s, db, teardown, err := testStore()
	if err != nil {
		t.Fatal(err)
	}
	defer teardown()

	dbtx, err := db.BeginReadWriteTx()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dbtx.Commit() }()
	ns := dbtx.ReadWriteBucket(namespaceKey)

	var category [32]byte
	copy(category[:], bytes.Repeat([]byte{0xab}, 32))
	amount := uint64(1000)
	token, err := wire.NewTokenData(category, &amount, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// A P2PKH output without tokens and one with fungible tokens.
	pkScript := append(append([]byte{0x76, 0xa9, 0x14}, bytes.Repeat([]byte{0x01}, 20)...), 0x88, 0xac)
	tx := spendOutput(&chainhash.Hash{0x01}, 0, 5000, 1000)
	tx.TxOut[0].PkScript = pkScript
	tx.TxOut[1].PkScript = pkScript
	tx.TxOut[1].TokenData = *token

	rec, err := NewTxRecordFromMsgTx(tx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertTx(ns, rec, TstRecvTxBlockDetails); err != nil {
		t.Fatal(err)
	}
	for i := uint32(0); i < 2; i++ {
		if err := s.AddCredit(ns, rec, TstRecvTxBlockDetails, i, false); err != nil {
			t.Fatal(err)
		}
	}

	unspents, err := s.UnspentOutputs(ns)
	if err != nil {
		t.Fatal(err)
	}
	if len(unspents) != 2 {
		t.Fatalf("got %d unspent outputs, want 2", len(unspents))
	}
	for _, c := range unspents {
		switch c.Index {
		case 0:
			if c.HasTokens() {
				t.Errorf("output 0 has tokens %+v", c.TokenData)
			}
		case 1:
			if !c.HasTokens() {
				t.Fatal("output 1 lost its tokens")
			}
			if c.TokenData.CategoryID != category || c.TokenData.Amount != amount {
				t.Errorf("output 1 tokens %+v, want %d of %x",
					c.TokenData, amount, category)
			}
		}
		if !bytes.Equal(c.PkScript, pkScript) {
			t.Errorf("output %d pkScript %x, want %x", c.Index,
				c.PkScript, pkScript)
		}
	}
}
