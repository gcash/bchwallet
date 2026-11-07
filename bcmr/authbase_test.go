package bcmr

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
)

// genesisSpending returns a transaction whose input spends output 0 of the
// category with the script, after an input that spends something else.
func genesisSpending(category chainhash.Hash, sigScript []byte) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{0xee}}, []byte{txscript.OP_1}))
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: category}, sigScript))
	return tx
}

func pushes(t *testing.T, data ...[]byte) []byte {
	t.Helper()
	b := txscript.NewScriptBuilder()
	for _, d := range data {
		b.AddData(d)
	}
	script, err := b.Script()
	if err != nil {
		t.Fatal(err)
	}
	return script
}

func contains(scripts [][]byte, script []byte) bool {
	for _, s := range scripts {
		if bytes.Equal(s, script) {
			return true
		}
	}
	return false
}

// TestAuthbaseScriptP2PKH ensures the script of a P2PKH authbase is rebuilt from
// the public key, as the wallet that made it would have paid to it.
func TestAuthbaseScriptP2PKH(t *testing.T) {
	category := chainhash.Hash{1}
	for _, pubKey := range [][]byte{
		append([]byte{0x02}, bytes.Repeat([]byte{7}, 32)...),
		append([]byte{0x04}, bytes.Repeat([]byte{7}, 64)...),
	} {
		address, err := bchutil.NewAddressPubKeyHash(bchutil.Hash160(pubKey), &chaincfg.MainNetParams)
		if err != nil {
			t.Fatal(err)
		}
		want, err := txscript.PayToAddrScript(address)
		if err != nil {
			t.Fatal(err)
		}
		sig := bytes.Repeat([]byte{9}, 65)
		scripts := authbaseScripts(genesisSpending(category, pushes(t, sig, pubKey)), category)
		if !contains(scripts, want) {
			t.Fatalf("%x not in %x", want, scripts)
		}
	}
}

// TestAuthbaseScriptP2SH ensures the script of a P2SH authbase is rebuilt from the
// redeem script, by both its hashes.
func TestAuthbaseScriptP2SH(t *testing.T) {
	category := chainhash.Hash{2}
	redeem := []byte{txscript.OP_1, txscript.OP_DROP, txscript.OP_TRUE}
	scripts := authbaseScripts(genesisSpending(category, pushes(t, []byte{1, 2}, redeem)), category)

	address, err := bchutil.NewAddressScriptHash(redeem, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	want, err := txscript.PayToAddrScript(address)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(scripts, want) {
		t.Fatalf("P2SH: %x not in %x", want, scripts)
	}
	first := sha256.Sum256(redeem)
	second := sha256.Sum256(first[:])
	want32 := append(append([]byte{txscript.OP_HASH256, txscript.OP_DATA_32}, second[:]...), txscript.OP_EQUAL)
	if !contains(scripts, want32) {
		t.Fatalf("P2SH32: %x not in %x", want32, scripts)
	}
	// Two pushes whose last isn't shaped as a key give no P2PKH.
	if len(scripts) != 2 {
		t.Fatalf("%d scripts", len(scripts))
	}
	// Nor do more than two, whatever the last looks like, as P2PKH is spent
	// with a signature and a key alone.
	key := append([]byte{0x02}, bytes.Repeat([]byte{7}, 32)...)
	scripts = authbaseScripts(genesisSpending(category, pushes(t, []byte{1, 2}, []byte{3, 4}, key)), category)
	if len(scripts) != 2 {
		t.Fatalf("%d scripts from three pushes", len(scripts))
	}
}

// TestAuthbaseScriptNone ensures nothing is rebuilt from what can't be read: no
// input that spends the authbase, nothing pushed, or more than pushes.
func TestAuthbaseScriptNone(t *testing.T) {
	category := chainhash.Hash{3}
	for name, tx := range map[string]*wire.MsgTx{
		"other input":   genesisSpending(chainhash.Hash{4}, pushes(t, []byte{1})),
		"empty":         genesisSpending(category, nil),
		"not only push": genesisSpending(category, []byte{txscript.OP_DUP, txscript.OP_DATA_1, 1}),
		"empty push":    genesisSpending(category, []byte{txscript.OP_0}),
	} {
		if scripts := authbaseScripts(tx, category); scripts != nil {
			t.Fatalf("%s: %x", name, scripts)
		}
	}
	// Output 1 of the authbase isn't the one a genesis spends.
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: category, Index: 1}, pushes(t, []byte{1, 2})))
	if scripts := authbaseScripts(tx, category); scripts != nil {
		t.Fatalf("output 1: %x", scripts)
	}
}
