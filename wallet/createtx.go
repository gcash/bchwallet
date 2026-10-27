// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"bytes"
	"errors"
	"fmt"
	"sort"

	"github.com/gcash/bchd/bchec"
	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/waddrmgr"
	"github.com/gcash/bchwallet/wallet/txauthor"
	"github.com/gcash/bchwallet/wallet/txrules"
	"github.com/gcash/bchwallet/walletdb"
	"github.com/gcash/bchwallet/wtxmgr"
)

// byAmount defines the methods needed to satisify sort.Interface to
// sort credits by their output amount.
type byAmount []wtxmgr.Credit

func (s byAmount) Len() int           { return len(s) }
func (s byAmount) Less(i, j int) bool { return s[i].Amount < s[j].Amount }
func (s byAmount) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

func makeInputSource(eligible []wtxmgr.Credit) txauthor.InputSource {
	// Pick largest outputs first.  This is only done for compatibility with
	// previous tx creation code, not because it's a good idea.
	sort.Sort(sort.Reverse(byAmount(eligible)))

	// Current inputs and their total value.  These are closed over by the
	// returned input source and reused across multiple calls.
	currentTotal := bchutil.Amount(0)
	currentInputs := make([]*wire.TxIn, 0, len(eligible))
	currentScripts := make([][]byte, 0, len(eligible))
	currentInputValues := make([]bchutil.Amount, 0, len(eligible))

	return func(target bchutil.Amount) (bchutil.Amount, []*wire.TxIn,
		[]bchutil.Amount, [][]byte, error) {

		for currentTotal < target && len(eligible) != 0 {
			nextCredit := &eligible[0]
			eligible = eligible[1:]
			nextInput := wire.NewTxIn(&nextCredit.OutPoint, nil)
			currentTotal += nextCredit.Amount
			currentInputs = append(currentInputs, nextInput)
			currentScripts = append(currentScripts, nextCredit.PkScript)
			currentInputValues = append(currentInputValues, nextCredit.Amount)
		}
		return currentTotal, currentInputs, currentInputValues, currentScripts, nil
	}
}

// secretSource is an implementation of txauthor.SecretSource for the wallet's
// address manager.
type secretSource struct {
	*waddrmgr.Manager
	addrmgrNs walletdb.ReadBucket
}

func (s secretSource) GetKey(addr bchutil.Address) (*bchec.PrivateKey, bool, error) {
	ma, err := s.Address(s.addrmgrNs, addr)
	if err != nil {
		return nil, false, err
	}

	mpka, ok := ma.(waddrmgr.ManagedPubKeyAddress)
	if !ok {
		e := fmt.Errorf("managed address type for %v is `%T` but "+
			"want waddrmgr.ManagedPubKeyAddress", addr, ma)
		return nil, false, e
	}
	privKey, err := mpka.PrivKey()
	if err != nil {
		return nil, false, err
	}
	return privKey, ma.Compressed(), nil
}

func (s secretSource) GetScript(addr bchutil.Address) ([]byte, error) {
	ma, err := s.Address(s.addrmgrNs, addr)
	if err != nil {
		return nil, err
	}

	msa, ok := ma.(waddrmgr.ManagedScriptAddress)
	if !ok {
		e := fmt.Errorf("managed address type for %v is `%T` but "+
			"want waddrmgr.ManagedScriptAddress", addr, ma)
		return nil, e
	}
	return msa.Script()
}

// txToOutputs creates a signed transaction which includes each output from
// outputs.  Previous outputs to reedeem are chosen from the passed account's
// UTXO set and minconf policy. An additional output may be added to return
// change to the wallet.  An appropriate fee is included based on the wallet's
// current relay fee.  The wallet must be unlocked to create the transaction.
//
// NOTE: The dryRun argument can be set true to create a tx that doesn't alter
// the database. A tx created with this set to true will intentionally have no
// input scripts added and SHOULD NOT be broadcasted.
func (w *Wallet) txToOutputs(outputs []*wire.TxOut, account uint32,
	minconf int32, feeSatPerKb bchutil.Amount, dryRun bool) (
	tx *txauthor.AuthoredTx, err error) {

	chainClient, err := w.requireChainClient()
	if err != nil {
		return nil, err
	}

	dbtx, err := w.db.BeginReadWriteTx()
	if err != nil {
		return nil, err
	}
	defer func() { _ = dbtx.Rollback() }()

	addrmgrNs := dbtx.ReadWriteBucket(waddrmgrNamespaceKey)

	// Get current block's height and hash.
	bs, err := chainClient.BlockStamp()
	if err != nil {
		return nil, err
	}

	eligible, err := w.findEligibleOutputs(dbtx, account, minconf, bs)
	if err != nil {
		return nil, err
	}

	inputSource := makeInputSource(eligible)
	changeSource := func() ([]byte, error) {
		// Derive the change output script.  As a hack to allow
		// spending from the imported account, change addresses are
		// created from account 0.
		var changeAddr bchutil.Address
		var err error
		if account == waddrmgr.ImportedAddrAccount {
			changeAddr, err = w.newChangeAddress(addrmgrNs, 0)
		} else {
			changeAddr, err = w.newChangeAddress(addrmgrNs, account)
		}
		if err != nil {
			return nil, err
		}
		return txscript.PayToAddrScript(changeAddr)
	}
	tx, err = txauthor.NewUnsignedTransaction(outputs, feeSatPerKb,
		inputSource, changeSource)
	if err != nil {
		return nil, err
	}

	// The signatures and the checks cover the tokens of the outputs the
	// inputs spend.
	tokens := make(map[wire.OutPoint]wire.TokenData, len(eligible))
	for i := range eligible {
		tokens[eligible[i].OutPoint] = eligible[i].TokenData
	}
	tx.PrevTokens = make([]wire.TokenData, len(tx.Tx.TxIn))
	for i, txIn := range tx.Tx.TxIn {
		tx.PrevTokens[i] = tokens[txIn.PreviousOutPoint]
	}

	// Randomize change position, if change exists, before signing.  This
	// doesn't affect the serialize size, so the change amount will still
	// be valid.
	if tx.ChangeIndex >= 0 {
		tx.RandomizeChangePosition()
	}

	// If a dry run was requested, we return now before adding the input
	// scripts, and don't commit the database transaction. The DB will be
	// rolled back when this method returns to ensure the dry run didn't
	// alter the DB in any way.
	if dryRun {
		return tx, nil
	}

	err = tx.AddAllInputScripts(secretSource{w.Manager, addrmgrNs})
	if err != nil {
		return nil, err
	}

	// The block stamp of the chain backend has no time, which the upgrades
	// that activate by time need, so the flags are those of the block the
	// wallet is synced to.
	syncedTo := w.Manager.SyncedTo()
	err = validateMsgTx(tx.Tx, tx.PrevScripts, tx.PrevInputValues,
		tx.PrevTokens, scriptFlags(w.chainParams, &syncedTo))
	if err != nil {
		return nil, err
	}

	if err := dbtx.Commit(); err != nil {
		return nil, err
	}

	if tx.ChangeIndex >= 0 && account == waddrmgr.ImportedAddrAccount {
		changeAmount := bchutil.Amount(tx.Tx.TxOut[tx.ChangeIndex].Value)
		log.Warnf("Spend from imported account produced change: moving"+
			" %v from imported account into default account.", changeAmount)
	}

	// Finally, we'll request the backend to notify us of the transaction
	// that pays to the change address, if there is one, when it confirms.
	if tx.ChangeIndex >= 0 {
		changePkScript := tx.Tx.TxOut[tx.ChangeIndex].PkScript
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(
			changePkScript, w.chainParams,
		)
		if err != nil {
			return nil, err
		}
		if err := chainClient.NotifyReceived(addrs); err != nil {
			return nil, err
		}
	}

	return tx, nil
}

// createUnsigned creates a unsigned transaction which includes each output from
// outputs.  Previous outputs to reedeem are chosen from the passed account's
// UTXO set and minconf policy. An additional output may be added to return
// change to the wallet.  An appropriate fee is included based on the wallet's
// current relay fee.  The wallet must be unlocked to create the transaction.
// The outputs may carry tokens, as CreateUnsignedTx describes.
func (w *Wallet) createUnsigned(outputs []*wire.TxOut, nfts []*wire.OutPoint,
	account uint32, minconf int32, feeSatPerKb bchutil.Amount) (
	tx *txauthor.AuthoredTx, err error) {

	chainClient, err := w.requireChainClient()
	if err != nil {
		return nil, err
	}

	err = walletdb.View(w.db, func(dbtx walletdb.ReadTx) error {

		// Get current block's height and hash.
		bs, err := chainClient.BlockStamp()
		if err != nil {
			return err
		}

		eligible, err := w.findEligibleOutputs(dbtx, account, minconf, bs)
		if err != nil {
			return err
		}

		changeSource := func() ([]byte, error) {
			// Derive the change output script.  As a hack to allow
			// spending from the imported account, change addresses
			// are created from account 0.
			var changeAddr bchutil.Address
			var err error
			if account == waddrmgr.ImportedAddrAccount {
				changeAddr, err = w.CurrentChangeAddress(0, waddrmgr.KeyScopeBIP0044)
			} else {
				changeAddr, err = w.CurrentChangeAddress(account, waddrmgr.KeyScopeBIP0044)
			}
			if err != nil {
				return nil, err
			}
			return txscript.PayToAddrScript(changeAddr)
		}

		// Outputs that carry tokens spend the account's outputs that
		// hold them, first, and the tokens left over return as change.
		var tokenCredits []wtxmgr.Credit
		if sendsTokens(outputs, nfts) {
			tokenEligible, err := w.findEligibleTokenOutputs(dbtx,
				account, minconf, bs)
			if err != nil {
				return err
			}
			changeScript, err := changeSource()
			if err != nil {
				return err
			}
			var tokenChange []*wire.TxOut
			tokenCredits, tokenChange, err = spendTokens(outputs, nfts,
				tokenEligible, changeScript)
			if err != nil {
				return err
			}
			outputs = append(outputs[:len(outputs):len(outputs)],
				tokenChange...)
		}

		inputSource := withInputs(tokenCredits, makeInputSource(eligible))
		tx, err = txauthor.NewUnsignedTransaction(outputs, feeSatPerKb,
			inputSource, changeSource)
		if err != nil {
			return err
		}
		tx.PrevTokens = make([]wire.TokenData, len(tx.Tx.TxIn))
		for i := range tokenCredits {
			tx.PrevTokens[i] = tokenCredits[i].TokenData
		}

		// Randomize change position, if change exists, before signing.
		// This doesn't affect the serialize size, so the change amount
		// will still be valid.
		if tx.ChangeIndex >= 0 {
			tx.RandomizeChangePosition()
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if tx.ChangeIndex >= 0 && account == waddrmgr.ImportedAddrAccount {
		changeAmount := bchutil.Amount(tx.Tx.TxOut[tx.ChangeIndex].Value)
		log.Warnf("Spend from imported account produced change: moving"+
			" %v from imported account into default account.", changeAmount)
	}

	return tx, nil
}

// ErrTokenSend is wrapped by the errors CreateUnsignedTx returns for outputs
// whose tokens the account can't send as asked.
var ErrTokenSend = errors.New("cannot send the tokens")

// sendsTokens returns whether any of outputs carries tokens, with the NFTs that
// nfts names for them.
func sendsTokens(outputs []*wire.TxOut, nfts []*wire.OutPoint) bool {
	for i, out := range outputs {
		if !out.TokenData.IsEmpty() || (i < len(nfts) && nfts[i] != nil) {
			return true
		}
	}
	return false
}

// spendTokens returns the outputs of eligible that a transaction with outputs
// spends for their tokens, and the outputs that return the tokens left over to
// changeScript: the fungible tokens of each category in one output, and each
// NFT spent only for the fungible tokens held with it in one of its own, as it
// is.  The NFT an output sends is the one held by the outpoint at the output's
// index in nfts, which spendTokens copies into the output.  The fungible
// tokens that the sent NFTs' outputs don't provide come from outputs without
// NFTs, largest first, and then from the other outputs with NFTs, so that all
// the fungible tokens TokenBalances counts can be sent.  An output with tokens
// and no value gets its dust threshold.
func spendTokens(outputs []*wire.TxOut, nfts []*wire.OutPoint,
	eligible []wtxmgr.Credit, changeScript []byte) ([]wtxmgr.Credit,
	[]*wire.TxOut, error) {

	byOutPoint := make(map[wire.OutPoint]*wtxmgr.Credit, len(eligible))
	for i := range eligible {
		byOutPoint[eligible[i].OutPoint] = &eligible[i]
	}

	var spent []wtxmgr.Credit
	have := make(map[chainhash.Hash]uint64)
	need := make(map[chainhash.Hash]uint64)
	for i, out := range outputs {
		token := &out.TokenData
		category := chainhash.Hash(token.CategoryID)
		if i < len(nfts) && nfts[i] != nil {
			op := *nfts[i]
			credit, ok := byOutPoint[op]
			switch {
			case !ok:
				return nil, nil, fmt.Errorf("%w: %v is not an "+
					"unspent output of the account, or is sent "+
					"twice", ErrTokenSend, op)
			case !credit.TokenData.HasNFT():
				return nil, nil, fmt.Errorf("%w: %v holds no NFT",
					ErrTokenSend, op)
			case credit.TokenData.CategoryID != token.CategoryID:
				return nil, nil, fmt.Errorf("%w: the NFT at %v is "+
					"of category %v, not %v", ErrTokenSend, op,
					chainhash.Hash(credit.TokenData.CategoryID),
					category)
			}
			delete(byOutPoint, op)
			spent = append(spent, *credit)
			if credit.TokenData.HasAmount() {
				have[category] += credit.TokenData.Amount
			}

			// The output carries the NFT as it is, with the fungible
			// amount it asks for.
			token.BitField = credit.TokenData.BitField &^ wire.HAS_AMOUNT
			if token.Amount > 0 {
				token.BitField |= wire.HAS_AMOUNT
			}
			token.Commitment = credit.TokenData.Commitment
		}
		if token.HasAmount() {
			if token.Amount > wire.MAX_FT_AMOUNT-need[category] {
				return nil, nil, fmt.Errorf("%w: the outputs send "+
					"more tokens of category %v than can exist",
					ErrTokenSend, category)
			}
			need[category] += token.Amount
		}
	}

	// The fungible tokens still needed come from the outputs that hold
	// only fungible tokens, and then from those that hold them with an NFT,
	// which goes back to the wallet in an output of its own.
	var fungible, withNFT []*wtxmgr.Credit
	for _, credit := range byOutPoint {
		if !credit.TokenData.HasAmount() ||
			need[credit.TokenData.CategoryID] == 0 {

			continue
		}
		if credit.TokenData.HasNFT() {
			withNFT = append(withNFT, credit)
		} else {
			fungible = append(fungible, credit)
		}
	}
	sortLargestTokens(fungible)
	sortLargestTokens(withNFT)
	for _, credit := range fungible {
		category := chainhash.Hash(credit.TokenData.CategoryID)
		if have[category] < need[category] {
			spent = append(spent, *credit)
			have[category] += credit.TokenData.Amount
		}
	}
	var nftChange []*wire.TxOut
	for _, credit := range withNFT {
		category := chainhash.Hash(credit.TokenData.CategoryID)
		if have[category] < need[category] {
			spent = append(spent, *credit)
			have[category] += credit.TokenData.Amount
			nftChange = append(nftChange, wire.NewTxOut(0, changeScript,
				wire.TokenData{
					CategoryID: credit.TokenData.CategoryID,
					Commitment: credit.TokenData.Commitment,
					BitField: credit.TokenData.BitField &^
						wire.HAS_AMOUNT,
				}))
		}
	}

	categories := make([]chainhash.Hash, 0, len(have)+len(need))
	for category := range need {
		categories = append(categories, category)
	}
	for category := range have {
		if _, ok := need[category]; !ok {
			categories = append(categories, category)
		}
	}
	sort.Slice(categories, func(i, j int) bool {
		return categories[i].String() < categories[j].String()
	})
	var change []*wire.TxOut
	for _, category := range categories {
		if have[category] < need[category] {
			return nil, nil, fmt.Errorf("%w: the account has %d tokens "+
				"of category %v to send %d", ErrTokenSend,
				have[category], category, need[category])
		}
		if left := have[category] - need[category]; left > 0 {
			change = append(change, wire.NewTxOut(0, changeScript,
				wire.TokenData{
					CategoryID: category,
					Amount:     left,
					BitField:   wire.HAS_AMOUNT,
				}))
		}
	}
	change = append(change, nftChange...)

	for _, out := range append(outputs[:len(outputs):len(outputs)], change...) {
		if out.TokenData.IsEmpty() {
			continue
		}
		threshold := txrules.OutputDustThreshold(out,
			txrules.DefaultRelayFeePerKb)
		switch {
		case out.Value == 0:
			out.Value = int64(threshold)
		case bchutil.Amount(out.Value) < threshold:
			return nil, nil, fmt.Errorf("%w: an output with these "+
				"tokens needs at least %v, not %v", ErrTokenSend,
				threshold, bchutil.Amount(out.Value))
		}
	}
	return spent, change, nil
}

// sortLargestTokens sorts credits by their fungible tokens, largest first, and
// then by outpoint, so that the order doesn't depend on the map they came from.
func sortLargestTokens(credits []*wtxmgr.Credit) {
	sort.Slice(credits, func(i, j int) bool {
		a, b := credits[i], credits[j]
		if a.TokenData.Amount != b.TokenData.Amount {
			return a.TokenData.Amount > b.TokenData.Amount
		}
		opA, opB := a.OutPoint, b.OutPoint
		if c := bytes.Compare(opA.Hash[:], opB.Hash[:]); c != 0 {
			return c < 0
		}
		return opA.Index < opB.Index
	})
}

// withInputs returns an input source that spends the credits, and then the
// inputs of source for the value they don't cover.
func withInputs(credits []wtxmgr.Credit,
	source txauthor.InputSource) txauthor.InputSource {

	if len(credits) == 0 {
		return source
	}
	var total bchutil.Amount
	inputs := make([]*wire.TxIn, len(credits))
	values := make([]bchutil.Amount, len(credits))
	scripts := make([][]byte, len(credits))
	for i := range credits {
		total += credits[i].Amount
		inputs[i] = wire.NewTxIn(&credits[i].OutPoint, nil)
		values[i] = credits[i].Amount
		scripts[i] = credits[i].PkScript
	}
	return func(target bchutil.Amount) (bchutil.Amount, []*wire.TxIn,
		[]bchutil.Amount, [][]byte, error) {

		more, moreInputs, moreValues, moreScripts, err := source(target - total)
		if err != nil {
			return 0, nil, nil, nil, err
		}
		return total + more,
			append(inputs[:len(inputs):len(inputs)], moreInputs...),
			append(values[:len(values):len(values)], moreValues...),
			append(scripts[:len(scripts):len(scripts)], moreScripts...),
			nil
	}
}

// findEligibleOutputs returns the account's unspent outputs without tokens that
// the wallet can spend with minconf confirmations at bs.
func (w *Wallet) findEligibleOutputs(dbtx walletdb.ReadTx, account uint32, minconf int32, bs *waddrmgr.BlockStamp) ([]wtxmgr.Credit, error) {
	return w.eligibleOutputs(dbtx, account, minconf, bs, false)
}

// findEligibleTokenOutputs is findEligibleOutputs for the outputs that carry
// tokens.
func (w *Wallet) findEligibleTokenOutputs(dbtx walletdb.ReadTx, account uint32, minconf int32, bs *waddrmgr.BlockStamp) ([]wtxmgr.Credit, error) {
	return w.eligibleOutputs(dbtx, account, minconf, bs, true)
}

// eligibleOutputs returns the account's unspent outputs that carry tokens if
// tokens is true, else those without, that the wallet can spend with minconf
// confirmations at bs.
func (w *Wallet) eligibleOutputs(dbtx walletdb.ReadTx, account uint32, minconf int32, bs *waddrmgr.BlockStamp, tokens bool) ([]wtxmgr.Credit, error) {
	addrmgrNs := dbtx.ReadBucket(waddrmgrNamespaceKey)
	txmgrNs := dbtx.ReadBucket(wtxmgrNamespaceKey)

	unspent, err := w.TxStore.UnspentOutputs(txmgrNs)
	if err != nil {
		return nil, err
	}

	// TODO: Eventually all of these filters (except perhaps output locking)
	// should be handled by the call to UnspentOutputs (or similar).
	// Because one of these filters requires matching the output script to
	// the desired account, this change depends on making wtxmgr a waddrmgr
	// dependancy and requesting unspent outputs for a single account.
	eligible := make([]wtxmgr.Credit, 0, len(unspent))
	for i := range unspent {
		output := &unspent[i]

		// Only include this output if it meets the required number of
		// confirmations.  Coinbase transactions must have have reached
		// maturity before their outputs may be spent.
		if !confirmed(minconf, output.Height, bs.Height) {
			continue
		}
		if output.FromCoinBase {
			target := int32(w.chainParams.CoinbaseMaturity)
			if !confirmed(target, output.Height, bs.Height) {
				continue
			}
		}

		// Locked unspent outputs are skipped.
		if w.LockedOutpoint(output.OutPoint) {
			continue
		}

		// Outputs carrying CashTokens are only spent to send their
		// tokens, since spending them otherwise burns the tokens.
		if output.HasTokens() != tokens {
			continue
		}

		// Only include the output if it is associated with the passed
		// account.
		//
		// TODO: Handle multisig outputs by determining if enough of the
		// addresses are controlled.
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(
			output.PkScript, w.chainParams)
		if err != nil || len(addrs) != 1 {
			continue
		}
		_, addrAcct, err := w.Manager.AddrAccount(addrmgrNs, addrs[0])
		if err != nil || addrAcct != account {
			continue
		}
		eligible = append(eligible, *output)
	}
	return eligible, nil
}

// scriptFlags returns the script flags bchd's memory pool checks a transaction
// against when the block after bs is next: the standard flags, with the rules
// of each upgrade that is active.  The time of the block stands in for the
// chain's median time past, which the wallet doesn't keep.
func scriptFlags(params *chaincfg.Params, bs *waddrmgr.BlockStamp) txscript.ScriptFlags {
	flags := txscript.StandardVerifyFlags
	if bs.Height+1 > params.Upgrade9ForkHeight {
		flags |= txscript.ScriptAllowCashTokens
	}
	if bs.Timestamp.Unix() >= int64(params.Upgrade11ActivationTime) {
		flags |= txscript.ScriptAllowMay2025 |
			txscript.ScriptAllowMay2025StandardOnly
	}
	if bs.Timestamp.Unix() >= int64(params.Upgrade12ActivationTime) {
		flags |= txscript.ScriptAllowMay2026
	}
	return flags
}

// validateMsgTx verifies transaction input scripts for tx, and the token rules
// if flags enable them.  The scripts, values and tokens of the outputs the
// transaction spends, in the order they are spent, must be passed in
// prevScripts, inputValues and prevTokens; prevTokens may be nil if no input
// spends tokens.
func validateMsgTx(tx *wire.MsgTx, prevScripts [][]byte, inputValues []bchutil.Amount,
	prevTokens []wire.TokenData, flags txscript.ScriptFlags) error {

	utxoCache := txscript.NewUtxoCache()
	for i, prevScript := range prevScripts {
		prevOut := wire.TxOut{
			Value:    int64(inputValues[i]),
			PkScript: prevScript,
		}
		if prevTokens != nil {
			prevOut.TokenData = prevTokens[i]
		}
		utxoCache.AddEntry(i, prevOut)
	}

	hashCache := txscript.NewTxSigHashes(tx)
	if flags.HasFlag(txscript.ScriptAllowCashTokens) {
		_, err := wire.RunCashTokensValidityAlgorithm(utxoCache, tx,
			flags.HasFlag(txscript.ScriptAllowMay2026))
		if err != nil {
			return fmt.Errorf("transaction breaks the token rules: %v",
				err)
		}
		hashCache = hashCache.WithUtxos(tx, utxoCache)
	}
	for i, prevScript := range prevScripts {
		vm, err := txscript.NewEngine(prevScript, tx, i,
			flags, nil, hashCache, utxoCache, int64(inputValues[i]))
		if err != nil {
			return fmt.Errorf("cannot create script engine: %s", err)
		}
		err = vm.Execute()
		if err != nil {
			return fmt.Errorf("cannot validate transaction: %s", err)
		}
	}
	return nil
}
