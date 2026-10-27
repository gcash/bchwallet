package wallet

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gcash/bchd/bchec"
	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchutil/hdkeychain"
	"github.com/gcash/bchwallet/waddrmgr"
	"github.com/gcash/bchwallet/wallet/txauthor"
	"github.com/gcash/bchwallet/walletdb"
	"github.com/gcash/bchwallet/wtxmgr"
)

// tokenFlags are the standard script flags with the token rules.
const tokenFlags = txscript.StandardVerifyFlags | txscript.ScriptAllowCashTokens

// testToken is a fungible token of a category that none of the tests' inputs
// can create.
var testToken = wire.TokenData{
	CategoryID: [32]byte{0xca, 0x7e},
	Amount:     500,
	BitField:   wire.HAS_AMOUNT,
}

// newRegtestWallet returns an unlocked regtest wallet, which applies the token
// rules from the start, with a mock chain backend.
func newRegtestWallet(t *testing.T) *Wallet {
	t.Helper()

	seed, err := hdkeychain.GenerateSeed(hdkeychain.MinSeedBytes)
	if err != nil {
		t.Fatal(err)
	}
	loader := NewLoader(&chaincfg.RegressionNetParams, t.TempDir(), true, 250)
	w, err := loader.CreateNewWallet([]byte("hello"), []byte("world"), seed,
		time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = loader.UnloadWallet() })
	w.chainClient = &mockChainClient{}
	if err := w.Unlock([]byte("world"), time.After(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	return w
}

// walletScript returns the output script of an address of the wallet.
func walletScript(t *testing.T, w *Wallet) []byte {
	t.Helper()

	addr, err := w.CurrentAddress(0, waddrmgr.KeyScopeBIP0044)
	if err != nil {
		t.Fatal(err)
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatal(err)
	}
	return script
}

// addCredit records a mined transaction paying out to the wallet and returns
// the outpoint of that output.
func addCredit(t *testing.T, w *Wallet, out *wire.TxOut) wire.OutPoint {
	t.Helper()

	rec, err := wtxmgr.NewTxRecordFromMsgTx(&wire.MsgTx{
		Version: wire.TxVersion,
		TxIn:    []*wire.TxIn{{}},
		TxOut:   []*wire.TxOut{out},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	block := &wtxmgr.BlockMeta{
		Block: wtxmgr.Block{Hash: chainhash.Hash{1}, Height: 1},
		Time:  time.Now(),
	}
	err = walletdb.Update(w.db, func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadWriteBucket(wtxmgrNamespaceKey)
		if err := w.TxStore.InsertTx(ns, rec, block); err != nil {
			return err
		}
		return w.TxStore.AddCredit(ns, rec, block, 0, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	return wire.OutPoint{Hash: rec.Hash, Index: 0}
}

// recorded returns whether the wallet has a record of tx.
func recorded(t *testing.T, w *Wallet, tx *wire.MsgTx) bool {
	t.Helper()

	hash := tx.TxHash()
	var details *wtxmgr.TxDetails
	err := walletdb.View(w.db, func(dbtx walletdb.ReadTx) error {
		var err error
		ns := dbtx.ReadBucket(wtxmgrNamespaceKey)
		details, err = w.TxStore.TxDetails(ns, &hash)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return details != nil
}

// spendTx returns a transaction spending ops to outs.
func spendTx(ops []wire.OutPoint, outs ...*wire.TxOut) *wire.MsgTx {
	tx := &wire.MsgTx{Version: wire.TxVersion, TxOut: outs}
	for i := range ops {
		tx.AddTxIn(wire.NewTxIn(&ops[i], nil))
	}
	return tx
}

// verifyInputs runs the scripts of tx's inputs with flags, given the outputs
// they spend, and returns the first error.
func verifyInputs(tx *wire.MsgTx, prevOuts []*wire.TxOut,
	flags txscript.ScriptFlags) error {

	utxos := txscript.NewUtxoCache()
	for i, prevOut := range prevOuts {
		utxos.AddEntry(i, *prevOut)
	}
	for i, prevOut := range prevOuts {
		vm, err := txscript.NewEngine(prevOut.PkScript, tx, i, flags, nil,
			nil, utxos, prevOut.Value)
		if err == nil {
			err = vm.Execute()
		}
		if err != nil {
			return fmt.Errorf("input %d: %v", i, err)
		}
	}
	return nil
}

// TestSignTransactionTokens checks that SignTransaction's signatures cover the
// tokens of the wallet's outputs it spends, taking those outputs from the
// wallet's records rather than the caller's values.
func TestSignTransactionTokens(t *testing.T) {
	w := newRegtestWallet(t)
	script := walletScript(t, w)
	tokenOut := wire.NewTxOut(1000, script, testToken)
	bchOut := wire.NewTxOut(100000, script, wire.TokenData{})
	ops := []wire.OutPoint{addCredit(t, w, tokenOut), addCredit(t, w, bchOut)}

	tests := []struct {
		name        string
		inputValues []int64
		prevScripts map[wire.OutPoint][]byte
	}{
		{name: "looked up"},
		{
			name:        "wrong caller values",
			inputValues: []int64{1, 1},
			prevScripts: map[wire.OutPoint][]byte{
				ops[0]: {txscript.OP_TRUE},
			},
		},
	}
	for _, test := range tests {
		tx := spendTx(ops, wire.NewTxOut(1000, script, testToken),
			wire.NewTxOut(90000, script, wire.TokenData{}))
		sigErrs, err := w.SignTransaction(tx, test.inputValues,
			txscript.SigHashAll, test.prevScripts, nil, nil)
		if err != nil || len(sigErrs) != 0 {
			t.Errorf("%s: got %v, %v", test.name, sigErrs, err)
			continue
		}
		err = verifyInputs(tx, []*wire.TxOut{tokenOut, bchOut}, tokenFlags)
		if err != nil {
			t.Errorf("%s: %v", test.name, err)
		}
	}
}

// TestSignTransactionCheckTokens checks that SignTransaction checks signatures
// with the token rules, so it reports a signature that leaves out the tokens
// its input spends.
func TestSignTransactionCheckTokens(t *testing.T) {
	w := newRegtestWallet(t)
	script := walletScript(t, w)
	tokenOut := wire.NewTxOut(1000, script, testToken)
	ops := []wire.OutPoint{
		addCredit(t, w, wire.NewTxOut(100000, script, wire.TokenData{})),
		addCredit(t, w, tokenOut),
	}

	// With one output, SIGHASH_SINGLE leaves the second input as it is
	// and only checks it.  Its signature leaves out its tokens.
	tx := spendTx(ops, wire.NewTxOut(100000, script, testToken))
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(script, w.ChainParams())
	if err != nil {
		t.Fatal(err)
	}
	wif, err := w.DumpWIFPrivateKey(addrs[0])
	if err != nil {
		t.Fatal(err)
	}
	key, err := bchutil.DecodeWIF(wif)
	if err != nil {
		t.Fatal(err)
	}
	tx.TxIn[1].SignatureScript, err = txscript.SignatureScript(tx, 1,
		tokenOut.Value, script, txscript.SigHashAll, key.PrivKey, true)
	if err != nil {
		t.Fatal(err)
	}

	sigErrs, err := w.SignTransaction(tx, nil, txscript.SigHashSingle, nil,
		nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sigErrs) != 1 || sigErrs[0].InputIndex != 1 {
		t.Fatalf("got %v, want an error for input 1", sigErrs)
	}
}

// TestSignTransactionSigHashUTXOs checks that SignTransaction signs with
// SIGHASH_UTXOS when it has every output the transaction spends, and refuses
// to otherwise.
func TestSignTransactionSigHashUTXOs(t *testing.T) {
	w := newRegtestWallet(t)
	script := walletScript(t, w)
	tokenOut := wire.NewTxOut(1000, script, testToken)
	bchOut := wire.NewTxOut(100000, script, wire.TokenData{})
	ops := []wire.OutPoint{addCredit(t, w, tokenOut), addCredit(t, w, bchOut)}
	hashType := txscript.SigHashAll | txscript.SigHashUTXO

	tx := spendTx(ops, wire.NewTxOut(1000, script, testToken),
		wire.NewTxOut(90000, script, wire.TokenData{}))
	sigErrs, err := w.SignTransaction(tx, nil, hashType, nil, nil, nil)
	if err != nil || len(sigErrs) != 0 {
		t.Fatalf("got %v, %v", sigErrs, err)
	}
	err = verifyInputs(tx, []*wire.TxOut{tokenOut, bchOut}, tokenFlags)
	if err != nil {
		t.Fatal(err)
	}

	// The wallet doesn't have the second output, whose tokens the
	// caller's script and amount can't show.
	foreign := wire.OutPoint{Hash: chainhash.Hash{9}, Index: 1}
	tx = spendTx([]wire.OutPoint{ops[1], foreign},
		wire.NewTxOut(90000, script, wire.TokenData{}))
	_, err = w.SignTransaction(tx, []int64{100000, 5000}, hashType,
		map[wire.OutPoint][]byte{foreign: script}, nil, nil)
	if err == nil {
		t.Fatal("signed with SIGHASH_UTXOS without every output the " +
			"transaction spends")
	}
}

// keySource is a txauthor.SecretsSource with one key.
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

// TestValidateMsgTxTokens checks that validateMsgTx applies the token rules
// when its flags enable them: signatures must cover the tokens their inputs
// spend, and outputs can't carry tokens that don't come from the inputs.
func TestValidateMsgTxTokens(t *testing.T) {
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

	prevScripts := [][]byte{script}
	values := []bchutil.Amount{1000}
	tokens := []wire.TokenData{testToken}
	newTx := func() *wire.MsgTx {
		op := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 1}
		return spendTx([]wire.OutPoint{op},
			wire.NewTxOut(900, script, testToken))
	}

	tx := newTx()
	err = txauthor.AddAllInputScripts(tx, prevScripts, values, keySource{key})
	if err != nil {
		t.Fatal(err)
	}
	if validateMsgTx(tx, prevScripts, values, tokens, tokenFlags) == nil {
		t.Error("accepted a signature that leaves out the tokens its " +
			"input spends")
	}

	tx = newTx()
	err = txauthor.AddAllInputScriptsWithTokens(tx, prevScripts, values,
		tokens, keySource{key})
	if err != nil {
		t.Fatal(err)
	}
	err = validateMsgTx(tx, prevScripts, values, tokens, tokenFlags)
	if err != nil {
		t.Errorf("rejected a valid token transaction: %v", err)
	}

	// The same output from an input without tokens.
	tx = newTx()
	err = txauthor.AddAllInputScripts(tx, prevScripts, values, keySource{key})
	if err != nil {
		t.Fatal(err)
	}
	err = validateMsgTx(tx, prevScripts, values, nil, tokenFlags)
	if err == nil || !strings.Contains(err.Error(), "token rules") {
		t.Errorf("got %v, want a token rules error", err)
	}
}

// TestPublishTransactionTokenRules checks that the wallet refuses to publish a
// transaction that breaks the token rules, before recording it.
func TestPublishTransactionTokenRules(t *testing.T) {
	w := newRegtestWallet(t)
	script := walletScript(t, w)
	op := addCredit(t, w, wire.NewTxOut(100000, script, wire.TokenData{}))

	// The tokens come from nothing: the spent output has none, and they
	// aren't of the category that spending it could create.
	bad := spendTx([]wire.OutPoint{op}, wire.NewTxOut(1000, script, testToken))
	err := w.PublishTransaction(bad)
	if err == nil || !strings.Contains(err.Error(), "token rules") {
		t.Fatalf("got %v, want a token rules error", err)
	}
	if recorded(t, w, bad) {
		t.Fatal("recorded a transaction that breaks the token rules")
	}

	good := spendTx([]wire.OutPoint{op},
		wire.NewTxOut(90000, script, wire.TokenData{}))
	if err := w.PublishTransaction(good); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, w, good) {
		t.Fatal("didn't record a valid transaction")
	}
}
