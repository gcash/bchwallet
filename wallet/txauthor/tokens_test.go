package txauthor_test

import (
	"errors"
	"testing"

	"github.com/gcash/bchd/bchec"
	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	. "github.com/gcash/bchwallet/wallet/txauthor"
)

// keySource is a SecretsSource with one key.
type keySource struct {
	key *bchec.PrivateKey
}

func (s keySource) GetKey(bchutil.Address) (*bchec.PrivateKey, bool, error) {
	return s.key, true, nil
}

func (s keySource) GetScript(bchutil.Address) ([]byte, error) {
	return nil, errors.New("no scripts")
}

func (s keySource) ChainParams() *chaincfg.Params {
	return &chaincfg.RegressionNetParams
}

// TestAddAllInputScriptsTokens checks that an authored transaction's
// signatures cover the tokens of the outputs its inputs spend, as nodes
// require, and that an input without tokens is signed as before.
func TestAddAllInputScriptsTokens(t *testing.T) {
	key, err := bchec.NewPrivateKey(bchec.S256())
	if err != nil {
		t.Fatal(err)
	}
	addr, err := bchutil.NewAddressPubKeyHash(
		bchutil.Hash160(key.PubKey().SerializeCompressed()),
		&chaincfg.RegressionNetParams)
	if err != nil {
		t.Fatal(err)
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatal(err)
	}

	token := wire.TokenData{
		CategoryID: [32]byte{0xca, 0x7e},
		Amount:     500,
		BitField:   wire.HAS_AMOUNT,
	}
	authored := &AuthoredTx{
		Tx: &wire.MsgTx{
			Version: wire.TxVersion,
			TxIn: []*wire.TxIn{
				wire.NewTxIn(&wire.OutPoint{Hash: [32]byte{1}, Index: 1}, nil),
				wire.NewTxIn(&wire.OutPoint{Hash: [32]byte{2}, Index: 1}, nil),
			},
			TxOut: []*wire.TxOut{
				wire.NewTxOut(1000, script, token),
				wire.NewTxOut(90000, script, wire.TokenData{}),
			},
		},
		PrevScripts:     [][]byte{script, script},
		PrevInputValues: []bchutil.Amount{1000, 100000},
		PrevTokens:      []wire.TokenData{token, {}},
	}
	unaware := authored.Tx.Copy()
	if err := authored.AddAllInputScripts(keySource{key}); err != nil {
		t.Fatal(err)
	}

	utxos := txscript.NewUtxoCache()
	for i := range authored.Tx.TxIn {
		utxos.AddEntry(i, *wire.NewTxOut(int64(authored.PrevInputValues[i]),
			script, authored.PrevTokens[i]))
	}
	verify := func(tx *wire.MsgTx, i int, flags txscript.ScriptFlags) error {
		vm, err := txscript.NewEngine(script, tx, i, flags, nil, nil,
			utxos, int64(authored.PrevInputValues[i]))
		if err != nil {
			return err
		}
		return vm.Execute()
	}
	tokenFlags := txscript.StandardVerifyFlags | txscript.ScriptAllowCashTokens
	for i := range authored.Tx.TxIn {
		if err := verify(authored.Tx, i, tokenFlags); err != nil {
			t.Errorf("input %d: %v", i, err)
		}
	}
	err = verify(authored.Tx, 1, txscript.StandardVerifyFlags)
	if err != nil {
		t.Errorf("input without tokens fails the rules from before "+
			"tokens: %v", err)
	}

	// AddAllInputScripts doesn't know the tokens, so its signature of
	// the input that spends them is invalid.
	err = AddAllInputScripts(unaware, authored.PrevScripts,
		authored.PrevInputValues, keySource{key})
	if err != nil {
		t.Fatal(err)
	}
	if verify(unaware, 0, tokenFlags) == nil {
		t.Error("AddAllInputScripts signed an input that spends tokens " +
			"validly")
	}
}
