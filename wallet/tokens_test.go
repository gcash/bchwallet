package wallet

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
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
	"github.com/gcash/bchwallet/wallet/txrules"
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

// testBlock is the block the tests' transactions are mined in.
var testBlock = &wtxmgr.BlockMeta{
	Block: wtxmgr.Block{Hash: chainhash.Hash{1}, Height: 1},
	Time:  time.Unix(1700000000, 0),
}

// insertTx records tx, mined in block, or unmined if block is nil, with its
// outputs at the credit indexes as the wallet's.
func insertTx(t *testing.T, w *Wallet, tx *wire.MsgTx, block *wtxmgr.BlockMeta,
	credits ...uint32) *wtxmgr.TxRecord {

	t.Helper()

	rec, err := wtxmgr.NewTxRecordFromMsgTx(tx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	err = walletdb.Update(w.db, func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadWriteBucket(wtxmgrNamespaceKey)
		if err := w.TxStore.InsertTx(ns, rec, block); err != nil {
			return err
		}
		for _, index := range credits {
			err := w.TxStore.AddCredit(ns, rec, block, index, false)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// addCredit records a mined transaction paying out to the wallet and returns
// the outpoint of that output.
func addCredit(t *testing.T, w *Wallet, out *wire.TxOut) wire.OutPoint {
	t.Helper()

	rec := insertTx(t, w, &wire.MsgTx{
		Version: wire.TxVersion,
		TxIn:    []*wire.TxIn{{}},
		TxOut:   []*wire.TxOut{out},
	}, testBlock, 0)
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
			txscript.SigHashAll, test.prevScripts, nil, nil, false)
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
		nil, nil, false)
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
	sigErrs, err := w.SignTransaction(tx, nil, hashType, nil, nil, nil, false)
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
		map[wire.OutPoint][]byte{foreign: script}, nil, nil, false)
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

// setSyncedTo marks the wallet synced to the block at height, so outputs mined
// up to it have confirmations.
func setSyncedTo(t *testing.T, w *Wallet, height int32) {
	t.Helper()

	err := walletdb.Update(w.db, func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadWriteBucket(waddrmgrNamespaceKey)
		return w.Manager.SetSyncedTo(ns, &waddrmgr.BlockStamp{
			Height:    height,
			Hash:      chainhash.Hash{byte(height)},
			Timestamp: time.Now(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// tokensEqual returns whether a and b hold the same tokens.
func tokensEqual(a, b *wire.TokenData) bool {
	return a.CategoryID == b.CategoryID && a.Amount == b.Amount &&
		a.BitField == b.BitField && bytes.Equal(a.Commitment, b.Commitment)
}

// tokenBalancesEqual returns whether a and b are the same balances.
func tokenBalancesEqual(a, b []TokenBalance) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Category != b[i].Category || a[i].Amount != b[i].Amount ||
			len(a[i].NFTs) != len(b[i].NFTs) {
			return false
		}
		for j := range a[i].NFTs {
			x, y := &a[i].NFTs[j], &b[i].NFTs[j]
			if x.OutPoint != y.OutPoint || x.Capability != y.Capability ||
				!bytes.Equal(x.Commitment, y.Commitment) {
				return false
			}
		}
	}
	return true
}

// TestTokenBalances checks that TokenBalances sums the fungible amounts and
// lists the NFTs in the account's unspent outputs with the required
// confirmations, by category.
func TestTokenBalances(t *testing.T) {
	w := newRegtestWallet(t)
	setSyncedTo(t, w, testBlock.Height)
	script := walletScript(t, w)

	// The bytes of a sort after b's, and in transaction ID order before.
	a := [32]byte{0: 2, 31: 1}
	b := [32]byte{0: 1, 31: 2}
	fungible := func(category [32]byte, amount uint64) wire.TokenData {
		return wire.TokenData{CategoryID: category, Amount: amount,
			BitField: wire.HAS_AMOUNT}
	}
	addCredit(t, w, wire.NewTxOut(1000, script, fungible(a, 100)))
	addCredit(t, w, wire.NewTxOut(1001, script, fungible(a, 200)))
	nfts := []TokenNFT{
		{
			OutPoint: addCredit(t, w, wire.NewTxOut(1002, script,
				wire.TokenData{
					CategoryID: b,
					Commitment: []byte("nft"),
					BitField: wire.HAS_NFT | wire.HAS_COMMITMENT_LENGTH |
						wire.MUTABLE,
				})),
			Capability: wire.MUTABLE,
			Commitment: []byte("nft"),
		},
		{
			OutPoint: addCredit(t, w, wire.NewTxOut(1003, script,
				wire.TokenData{
					CategoryID: b,
					Amount:     5,
					BitField:   wire.HAS_AMOUNT | wire.HAS_NFT,
				})),
			Capability: wire.NONE,
		},
	}
	if bytes.Compare(nfts[0].OutPoint.Hash[:], nfts[1].OutPoint.Hash[:]) > 0 {
		nfts[0], nfts[1] = nfts[1], nfts[0]
	}
	addCredit(t, w, wire.NewTxOut(100000, script, wire.TokenData{}))

	// An unmined transaction, with an input of its own, as the store
	// drops unmined transactions that spend what a mined one does.
	unminedInput := wire.OutPoint{Hash: chainhash.Hash{7}, Index: 7}
	insertTx(t, w, spendTx([]wire.OutPoint{unminedInput},
		wire.NewTxOut(1004, script, fungible(a, 7))), nil, 0)

	// Tokens of another account.
	account, err := w.NextAccount(waddrmgr.KeyScopeBIP0044, "other")
	if err != nil {
		t.Fatal(err)
	}
	addr, err := w.NewAddress(account, waddrmgr.KeyScopeBIP0044)
	if err != nil {
		t.Fatal(err)
	}
	otherScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatal(err)
	}
	addCredit(t, w, wire.NewTxOut(1005, otherScript, fungible(a, 1000)))

	tests := []struct {
		confirms int32
		want     []TokenBalance
	}{
		{
			confirms: 1,
			want: []TokenBalance{
				{Category: a, Amount: 300},
				{Category: b, Amount: 5, NFTs: nfts},
			},
		},
		{
			confirms: 0,
			want: []TokenBalance{
				{Category: a, Amount: 307},
				{Category: b, Amount: 5, NFTs: nfts},
			},
		},
	}
	for _, test := range tests {
		got, err := w.TokenBalances(0, test.confirms)
		if err != nil {
			t.Fatal(err)
		}
		if !tokenBalancesEqual(got, test.want) {
			t.Errorf("%d confirmations: got %+v, want %+v",
				test.confirms, got, test.want)
		}
	}
}

// TestTxSummaryTokens checks that a transaction summary carries the tokens of
// the wallet's outputs and of the outputs its inputs spend.
func TestTxSummaryTokens(t *testing.T) {
	w := newRegtestWallet(t)
	script := walletScript(t, w)
	received := addCredit(t, w, wire.NewTxOut(1000, script, testToken))
	rec := insertTx(t, w, spendTx([]wire.OutPoint{received},
		wire.NewTxOut(900, script, testToken)), testBlock, 0)

	var summary TransactionSummary
	err := walletdb.View(w.db, func(dbtx walletdb.ReadTx) error {
		ns := dbtx.ReadBucket(wtxmgrNamespaceKey)
		details, err := w.TxStore.TxDetails(ns, &rec.Hash)
		if err != nil {
			return err
		}
		summary = makeTxSummary(dbtx, w, details)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.MyInputs) != 1 ||
		!tokensEqual(&summary.MyInputs[0].PreviousToken, &testToken) {
		t.Errorf("inputs: got %+v", summary.MyInputs)
	}
	if len(summary.MyOutputs) != 1 ||
		!tokensEqual(&summary.MyOutputs[0].Token, &testToken) {
		t.Errorf("outputs: got %+v", summary.MyOutputs)
	}
}

// spentBy returns the outputs of prevOuts that tx spends, in input order.
func spentBy(t *testing.T, tx *wire.MsgTx,
	prevOuts map[wire.OutPoint]*wire.TxOut) []*wire.TxOut {

	t.Helper()

	spent := make([]*wire.TxOut, len(tx.TxIn))
	for i, txIn := range tx.TxIn {
		spent[i] = prevOuts[txIn.PreviousOutPoint]
		if spent[i] == nil {
			t.Fatalf("input %d spends %v, which isn't the wallet's", i,
				txIn.PreviousOutPoint)
		}
	}
	return spent
}

// signAndVerify signs tx with the wallet, which must not refuse, and checks
// its inputs with the token rules, given the outputs they spend.
func signAndVerify(t *testing.T, w *Wallet, tx *wire.MsgTx,
	spent []*wire.TxOut) {

	t.Helper()

	sigErrs, err := w.SignTransaction(tx, nil, txscript.SigHashAll, nil,
		nil, nil, false)
	if err != nil || len(sigErrs) != 0 {
		t.Fatalf("signing: got %v, %v", sigErrs, err)
	}
	if err := verifyInputs(tx, spent, tokenFlags); err != nil {
		t.Fatal(err)
	}
}

// TestCreateUnsignedTokenTx checks that CreateUnsignedTx spends the account's
// outputs with the fungible tokens an output sends, largest first, returns the
// tokens left over as change, gives the outputs with tokens their dust
// threshold, and creates a transaction that signs validly under the token
// rules with enough fee.
func TestCreateUnsignedTokenTx(t *testing.T) {
	w := newRegtestWallet(t)
	script := walletScript(t, w)
	category := testToken.CategoryID
	fungible := func(amount uint64) wire.TokenData {
		return wire.TokenData{CategoryID: category, Amount: amount,
			BitField: wire.HAS_AMOUNT}
	}
	// The outputs with the tokens hold less than the dust thresholds of
	// the two outputs with tokens, so BCH is spent too.
	prevOuts := make(map[wire.OutPoint]*wire.TxOut)
	for _, out := range []*wire.TxOut{
		wire.NewTxOut(600, script, fungible(70)),
		wire.NewTxOut(601, script, fungible(50)),
		wire.NewTxOut(602, script, fungible(10)),
		wire.NewTxOut(100000, script, wire.TokenData{}),
	} {
		prevOuts[addCredit(t, w, out)] = out
	}

	tx, err := w.CreateUnsignedTx(0, []*wire.TxOut{
		wire.NewTxOut(0, script, fungible(100)),
	}, nil, 1, txrules.DefaultRelayFeePerKb)
	if err != nil {
		t.Fatal(err)
	}

	spent := spentBy(t, tx.Tx, prevOuts)
	var spentTokens []uint64
	for _, out := range spent {
		if !out.TokenData.IsEmpty() {
			spentTokens = append(spentTokens, out.TokenData.Amount)
		}
	}
	if len(spent) != 3 || fmt.Sprint(spentTokens) != "[70 50]" {
		t.Errorf("spent %d outputs, with tokens %v; want the outputs with "+
			"70 and 50 tokens, and one without", len(spent), spentTokens)
	}

	var sent []uint64
	for _, out := range tx.Tx.TxOut {
		if out.TokenData.IsEmpty() {
			continue
		}
		sent = append(sent, out.TokenData.Amount)
		threshold := txrules.OutputDustThreshold(out,
			txrules.DefaultRelayFeePerKb)
		if out.TokenData.CategoryID != category ||
			out.Value != int64(threshold) {

			t.Errorf("output of %d satoshis with %d tokens of %x; want "+
				"%v", out.Value, out.TokenData.Amount,
				out.TokenData.CategoryID, threshold)
		}
	}
	sort.Slice(sent, func(i, j int) bool { return sent[i] < sent[j] })
	if fmt.Sprint(sent) != "[20 100]" {
		t.Errorf("outputs with %v tokens, want 100 sent and 20 back", sent)
	}

	signAndVerify(t, w, tx.Tx, spent)
	var fee int64
	for _, out := range spent {
		fee += out.Value
	}
	for _, out := range tx.Tx.TxOut {
		fee -= out.Value
	}
	minFee := txrules.FeeForSerializeSize(txrules.DefaultRelayFeePerKb,
		tx.Tx.SerializeSize())
	if bchutil.Amount(fee) < minFee {
		t.Errorf("fee of %d satoshis, below the %v %d bytes need", fee,
			minFee, tx.Tx.SerializeSize())
	}
}

// TestCreateUnsignedNFTTx checks that CreateUnsignedTx sends the NFT an output
// names as it is, and returns the fungible tokens its output also held.
func TestCreateUnsignedNFTTx(t *testing.T) {
	w := newRegtestWallet(t)
	script := walletScript(t, w)
	category := testToken.CategoryID
	nftOut := wire.NewTxOut(1000, script, wire.TokenData{
		CategoryID: category,
		Amount:     5,
		Commitment: []byte("nft"),
		BitField: wire.HAS_AMOUNT | wire.HAS_NFT |
			wire.HAS_COMMITMENT_LENGTH | wire.MUTABLE,
	})
	bchOut := wire.NewTxOut(100000, script, wire.TokenData{})
	nft := addCredit(t, w, nftOut)
	prevOuts := map[wire.OutPoint]*wire.TxOut{
		nft:                     nftOut,
		addCredit(t, w, bchOut): bchOut,
	}

	tx, err := w.CreateUnsignedTx(0, []*wire.TxOut{
		wire.NewTxOut(0, script, wire.TokenData{CategoryID: category}),
	}, []*wire.OutPoint{&nft}, 1, txrules.DefaultRelayFeePerKb)
	if err != nil {
		t.Fatal(err)
	}

	sentNFT := wire.TokenData{
		CategoryID: category,
		Commitment: []byte("nft"),
		BitField:   wire.HAS_NFT | wire.HAS_COMMITMENT_LENGTH | wire.MUTABLE,
	}
	change := wire.TokenData{CategoryID: category, Amount: 5,
		BitField: wire.HAS_AMOUNT}
	var gotNFT, gotChange bool
	for _, out := range tx.Tx.TxOut {
		gotNFT = gotNFT || tokensEqual(&out.TokenData, &sentNFT)
		gotChange = gotChange || tokensEqual(&out.TokenData, &change)
	}
	if !gotNFT || !gotChange {
		t.Errorf("NFT sent %v, 5 tokens returned %v", gotNFT, gotChange)
	}
	signAndVerify(t, w, tx.Tx, spentBy(t, tx.Tx, prevOuts))
}

// TestCreateUnsignedTokenErrors checks the token sends CreateUnsignedTx
// refuses.
func TestCreateUnsignedTokenErrors(t *testing.T) {
	w := newRegtestWallet(t)
	script := walletScript(t, w)
	category := testToken.CategoryID
	fungible := addCredit(t, w, wire.NewTxOut(1000, script, testToken))
	nft := addCredit(t, w, wire.NewTxOut(1001, script,
		wire.TokenData{CategoryID: category, BitField: wire.HAS_NFT}))
	addCredit(t, w, wire.NewTxOut(100000, script, wire.TokenData{}))
	unknown := wire.OutPoint{Hash: chainhash.Hash{9}}

	send := func(value int64, token wire.TokenData) *wire.TxOut {
		return wire.NewTxOut(value, script, token)
	}
	nftOnly := wire.TokenData{CategoryID: category}
	tooMany := testToken
	tooMany.Amount++
	tests := []struct {
		name    string
		outputs []*wire.TxOut
		nfts    []*wire.OutPoint
	}{
		{"unknown NFT", []*wire.TxOut{send(0, nftOnly)},
			[]*wire.OutPoint{&unknown}},
		{"not an NFT", []*wire.TxOut{send(0, nftOnly)},
			[]*wire.OutPoint{&fungible}},
		{"other category", []*wire.TxOut{
			send(0, wire.TokenData{CategoryID: [32]byte{1}})},
			[]*wire.OutPoint{&nft}},
		{"NFT twice", []*wire.TxOut{send(0, nftOnly), send(0, nftOnly)},
			[]*wire.OutPoint{&nft, &nft}},
		{"too few tokens", []*wire.TxOut{send(0, tooMany)}, nil},
		{"below dust", []*wire.TxOut{send(600, testToken)}, nil},
	}
	for _, test := range tests {
		_, err := w.CreateUnsignedTx(0, test.outputs, test.nfts, 1,
			txrules.DefaultRelayFeePerKb)
		if !errors.Is(err, ErrTokenSend) {
			t.Errorf("%s: got %v, want ErrTokenSend", test.name, err)
		}
	}
}

// TestSignTransactionTokenBurn checks that SignTransaction refuses to sign a
// transaction that would burn tokens of the wallet's outputs it spends, unless
// burns are allowed.
func TestSignTransactionTokenBurn(t *testing.T) {
	w := newRegtestWallet(t)
	script := walletScript(t, w)
	nft := wire.TokenData{
		CategoryID: testToken.CategoryID,
		Commitment: []byte("nft"),
		BitField:   wire.HAS_NFT | wire.HAS_COMMITMENT_LENGTH,
	}
	ops := []wire.OutPoint{
		addCredit(t, w, wire.NewTxOut(1000, script, testToken)),
		addCredit(t, w, wire.NewTxOut(1001, script, nft)),
	}
	fewer := testToken
	fewer.Amount--

	tests := []struct {
		name  string
		outs  []*wire.TxOut
		burns bool
	}{
		{"keeps them", []*wire.TxOut{wire.NewTxOut(900, script, testToken),
			wire.NewTxOut(900, script, nft)}, false},
		{"fewer fungible", []*wire.TxOut{wire.NewTxOut(900, script, fewer),
			wire.NewTxOut(900, script, nft)}, true},
		{"no NFT", []*wire.TxOut{wire.NewTxOut(900, script, testToken)},
			true},
		{"no tokens", []*wire.TxOut{
			wire.NewTxOut(1800, script, wire.TokenData{})}, true},
	}
	for _, test := range tests {
		tx := spendTx(ops, test.outs...)
		_, err := w.SignTransaction(tx, nil, txscript.SigHashAll, nil,
			nil, nil, false)
		if errors.Is(err, ErrTokenBurn) != test.burns {
			t.Errorf("%s: got %v, want a burn %v", test.name, err,
				test.burns)
		}
		_, err = w.SignTransaction(tx, nil, txscript.SigHashAll, nil, nil,
			nil, true)
		if err != nil {
			t.Errorf("%s, with burns allowed: %v", test.name, err)
		}
	}
}
