package wtxmgr

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/walletdb"
)

// applyMigration is a helper function that allows us to assert the state of the
// top-level bucket before and after a migration. This can be used to ensure
// the correctness of migrations.
func applyMigration(t *testing.T,
	beforeMigration, afterMigration func(walletdb.ReadWriteBucket, *Store) error,
	migration func(walletdb.ReadWriteBucket) error, shouldFail bool) {

	t.Helper()

	// We'll start by setting up our transaction store backed by a database.
	store, db, teardown, err := testStore()
	if err != nil {
		t.Fatalf("unable to create test store: %v", err)
	}
	defer teardown()

	// First, we'll run the beforeMigration closure, which contains the
	// database modifications/assertions needed before proceeding with the
	// migration.
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(namespaceKey)
		if ns == nil {
			return errors.New("top-level namespace does not exist")
		}
		return beforeMigration(ns, store)
	})
	if err != nil {
		t.Fatalf("unable to run beforeMigration func: %v", err)
	}

	// Then, we'll run the migration itself and fail if it does not match
	// its expected result.
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(namespaceKey)
		if ns == nil {
			return errors.New("top-level namespace does not exist")
		}
		return migration(ns)
	})
	if err != nil && !shouldFail {
		t.Fatalf("unable to perform migration: %v", err)
	} else if err == nil && shouldFail {
		t.Fatal("expected migration to fail, but did not")
	}

	// Finally, we'll run the afterMigration closure, which contains the
	// assertions needed in order to guarantee than the migration was
	// successful.
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(namespaceKey)
		if ns == nil {
			return errors.New("top-level namespace does not exist")
		}
		return afterMigration(ns, store)
	})
	if err != nil {
		t.Fatalf("unable to run afterMigration func: %v", err)
	}
}

// TestMigrationDropTransactionHistory ensures that a transaction store is reset
// to a clean state after dropping its transaction history.
func TestMigrationDropTransactionHistory(t *testing.T) {
	t.Parallel()

	// checkTransactions is a helper function that will assert the correct
	// state of the transaction store based on whether the migration has
	// completed or not.
	checkTransactions := func(ns walletdb.ReadWriteBucket, s *Store,
		afterMigration bool) error {

		// We should see one confirmed unspent output before the
		// migration, and none after.
		utxos, err := s.UnspentOutputs(ns)
		if err != nil {
			return err
		}
		if len(utxos) == 0 && !afterMigration {
			return errors.New("expected to find 1 utxo, found none")
		}
		if len(utxos) > 0 && afterMigration {
			return fmt.Errorf("expected to find 0 utxos, found %d",
				len(utxos))
		}

		// We should see one unconfirmed transaction before the
		// migration, and none after.
		unconfirmedTxs, err := s.UnminedTxs(ns)
		if err != nil {
			return err
		}
		if len(unconfirmedTxs) == 0 && !afterMigration {
			return errors.New("expected to find 1 unconfirmed " +
				"transaction, found none")
		}
		if len(unconfirmedTxs) > 0 && afterMigration {
			return fmt.Errorf("expected to find 0 unconfirmed "+
				"transactions, found %d", len(unconfirmedTxs))
		}

		// We should have a non-zero balance before the migration, and
		// zero after.
		minedBalance, err := fetchMinedBalance(ns)
		if err != nil {
			return err
		}
		if minedBalance == 0 && !afterMigration {
			return errors.New("expected non-zero balance before " +
				"migration")
		}
		if minedBalance > 0 && afterMigration {
			return fmt.Errorf("expected zero balance after "+
				"migration, got %d", minedBalance)
		}

		return nil
	}

	beforeMigration := func(ns walletdb.ReadWriteBucket, s *Store) error {
		// We'll start by adding two transactions to the store: a
		// confirmed transaction and an unconfirmed transaction one.
		// The confirmed transaction will spend from a coinbase output,
		// while the unconfirmed will spend an output from the confirmed
		// transaction.
		cb := newCoinBase(1e8)
		cbRec, err := NewTxRecordFromMsgTx(cb, timeNow())
		if err != nil {
			return err
		}

		b := &BlockMeta{Block: Block{Height: 100}}
		confirmedSpend := spendOutput(&cbRec.Hash, 0, 5e7, 4e7)
		confirmedSpendRec, err := NewTxRecordFromMsgTx(
			confirmedSpend, timeNow(),
		)
		if err != nil {
			return err
		}
		if err := s.InsertTx(ns, confirmedSpendRec, b); err != nil {
			return err
		}
		err = s.AddCredit(ns, confirmedSpendRec, b, 1, true)
		if err != nil {
			return err
		}

		unconfimedSpend := spendOutput(
			&confirmedSpendRec.Hash, 0, 5e6, 5e6,
		)
		unconfirmedSpendRec, err := NewTxRecordFromMsgTx(
			unconfimedSpend, timeNow(),
		)
		if err != nil {
			return err
		}
		if err := s.InsertTx(ns, unconfirmedSpendRec, nil); err != nil {
			return err
		}
		err = s.AddCredit(ns, unconfirmedSpendRec, nil, 1, true)
		if err != nil {
			return err
		}

		// Ensure these transactions exist within the store.
		return checkTransactions(ns, s, false)
	}

	afterMigration := func(ns walletdb.ReadWriteBucket, s *Store) error {
		// Assuming the migration was successful, we should see that the
		// store no longer has the transaction history prior to the
		// migration.
		return checkTransactions(ns, s, true)
	}

	// We can now apply the migration and expect it not to fail.
	applyMigration(
		t, beforeMigration, afterMigration, dropTransactionHistory,
		false,
	)
}

// TestMigrationMarkCreditsSpentByMinedTxs ensures that a credit that a mined
// transaction spends is marked spent when the store kept it unspent, as it did
// for a transaction recorded before its parent in their block, and that the
// unmined transactions that spend it again are removed.
func TestMigrationMarkCreditsSpentByMinedTxs(t *testing.T) {
	t.Parallel()

	fund := spendOutput(&chainhash.Hash{1}, 0, 1e8)
	fundHash := fund.TxHash()
	parent := spendOutput(&fundHash, 0, 4e7, 5e7)
	parentHash := parent.TxHash()
	spend := spendOutput(&parentHash, 1, 4e7)
	spendHash := spend.TxHash()

	// again spends the output of the parent that spend spends, and
	// againChild spends the output of again.
	again := spendOutput(&parentHash, 1, 3e7)
	againHash := again.TxHash()
	againChild := spendOutput(&againHash, 0, 2e7)

	fundBlock := &BlockMeta{Block: Block{Hash: chainhash.Hash{100}, Height: 100}}
	block := &BlockMeta{Block: Block{Hash: chainhash.Hash{101}, Height: 101}}

	// insert records tx, mined in block or unmined if block is nil, with the
	// outputs at the credit indexes as the wallet's.
	insert := func(ns walletdb.ReadWriteBucket, s *Store, tx *wire.MsgTx,
		block *BlockMeta, credits ...uint32) error {

		rec, err := NewTxRecordFromMsgTx(tx, timeNow())
		if err != nil {
			return err
		}
		if err := s.InsertTx(ns, rec, block); err != nil {
			return err
		}
		for _, index := range credits {
			err := s.AddCredit(ns, rec, block, index, false)
			if err != nil {
				return err
			}
		}
		return nil
	}

	// checkStore returns an error unless the store has the mined balance
	// and the number of unmined transactions given.
	checkStore := func(ns walletdb.ReadWriteBucket, s *Store,
		balance bchutil.Amount, unmined int) error {

		minedBalance, err := fetchMinedBalance(ns)
		if err != nil {
			return err
		}
		if minedBalance != balance {
			return fmt.Errorf("mined balance %v, want %v",
				minedBalance, balance)
		}
		txs, err := s.UnminedTxs(ns)
		if err != nil {
			return err
		}
		if len(txs) != unmined {
			return fmt.Errorf("%d unmined transactions, want %d",
				len(txs), unmined)
		}
		return nil
	}

	tests := []struct {
		name string

		// spendFirst is whether the store records the spend before its
		// parent in their block.
		spendFirst bool

		// balance and unmined are the mined balance and the number of
		// unmined transactions before the migration.
		balance bchutil.Amount
		unmined int
	}{
		{
			name:       "spend recorded before its parent",
			spendFirst: true,
			balance:    13e7,
			unmined:    2,
		},
		{
			name:       "spend recorded after its parent",
			spendFirst: false,
			balance:    8e7,
			unmined:    0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			beforeMigration := func(ns walletdb.ReadWriteBucket,
				s *Store) error {

				if err := insert(ns, s, fund, fundBlock, 0); err != nil {
					return err
				}

				// The wallet sent the parent and the spend, so
				// it has them unmined until they're mined in
				// one block.
				if err := insert(ns, s, parent, nil, 0, 1); err != nil {
					return err
				}
				if err := insert(ns, s, spend, nil, 0); err != nil {
					return err
				}
				if test.spendFirst {
					err := insert(ns, s, spend, block, 0)
					if err != nil {
						return err
					}
				}
				if err := insert(ns, s, parent, block, 0, 1); err != nil {
					return err
				}
				if !test.spendFirst {
					err := insert(ns, s, spend, block, 0)
					if err != nil {
						return err
					}
				}

				// A store that kept the parent's output unspent
				// let the wallet spend it again.
				if test.spendFirst {
					err := insert(ns, s, again, nil, 0)
					if err != nil {
						return err
					}
					err = insert(ns, s, againChild, nil, 0)
					if err != nil {
						return err
					}
				}

				return checkStore(ns, s, test.balance, test.unmined)
			}

			afterMigration := func(ns walletdb.ReadWriteBucket,
				s *Store) error {

				if err := checkStore(ns, s, 8e7, 0); err != nil {
					return err
				}

				credits, err := s.UnspentOutputs(ns)
				if err != nil {
					return err
				}
				unspent := make(map[wire.OutPoint]bool)
				for _, credit := range credits {
					unspent[credit.OutPoint] = true
				}
				want := map[wire.OutPoint]bool{
					{Hash: parentHash, Index: 0}: true,
					{Hash: spendHash, Index: 0}:  true,
				}
				if !reflect.DeepEqual(unspent, want) {
					return fmt.Errorf("unspent outputs %v, "+
						"want %v", unspent, want)
				}

				details, err := s.TxDetails(ns, &spendHash)
				if err != nil {
					return err
				}
				wantDebits := []DebitRecord{{Amount: 5e7, Index: 0}}
				if !reflect.DeepEqual(details.Debits, wantDebits) {
					return fmt.Errorf("spend debits %v, want %v",
						details.Debits, wantDebits)
				}
				return nil
			}

			applyMigration(
				t, beforeMigration, afterMigration,
				markCreditsSpentByMinedTxs, false,
			)
		})
	}
}

// TestMigrationRemoveTxsOfOthers ensures that migrating a store removes the
// mined and unmined transactions that neither pay nor spend the wallet, with
// them in the records of their blocks, and keeps the wallet's.
func TestMigrationRemoveTxsOfOthers(t *testing.T) {
	t.Parallel()

	fund := spendOutput(&chainhash.Hash{1}, 0, 1e8)
	fundHash := fund.TxHash()
	payment := spendOutput(&fundHash, 0, 3e7, 6e7)
	paymentHash := payment.TxHash()

	// others spends the output that the payment paid someone, in a block
	// with a transaction paying the wallet, and othersLater spends its
	// output in a block of its own.
	others := spendOutput(&paymentHash, 0, 2e7)
	othersHash := others.TxHash()
	received := spendOutput(&chainhash.Hash{2}, 0, 1e7)
	othersLater := spendOutput(&othersHash, 0, 1e7)
	othersLaterHash := othersLater.TxHash()

	// unminedOthers spends the output of othersLater and unminedChild its
	// output, while unminedSpend spends the payment's change.
	unminedOthers := spendOutput(&othersLaterHash, 0, 9e6)
	unminedOthersHash := unminedOthers.TxHash()
	unminedChild := spendOutput(&unminedOthersHash, 0, 8e6)
	unminedSpend := spendOutput(&paymentHash, 1, 5e7)

	block := func(height int32) *BlockMeta {
		return &BlockMeta{Block: Block{
			Hash:   chainhash.Hash{byte(height)},
			Height: height,
		}}
	}
	records := []struct {
		tx      *wire.MsgTx
		block   *BlockMeta
		credits []uint32
		kept    bool
	}{
		{fund, block(100), []uint32{0}, true},
		{payment, block(101), []uint32{1}, true},
		{others, block(102), nil, false},
		{received, block(102), []uint32{0}, true},
		{othersLater, block(103), nil, false},
		{unminedOthers, nil, nil, false},
		{unminedChild, nil, nil, false},
		{unminedSpend, nil, nil, true},
	}

	beforeMigration := func(ns walletdb.ReadWriteBucket, s *Store) error {
		for _, r := range records {
			rec, err := NewTxRecordFromMsgTx(r.tx, timeNow())
			if err != nil {
				return err
			}
			if err := s.InsertTx(ns, rec, r.block); err != nil {
				return err
			}
			for _, index := range r.credits {
				err := s.AddCredit(ns, rec, r.block, index, false)
				if err != nil {
					return err
				}
			}
		}
		return nil
	}

	afterMigration := func(ns walletdb.ReadWriteBucket, s *Store) error {
		for i, r := range records {
			hash := r.tx.TxHash()
			details, err := s.TxDetails(ns, &hash)
			if err != nil {
				return err
			}
			if (details != nil) != r.kept {
				return fmt.Errorf("record %d kept: %v, want %v", i,
					details != nil, r.kept)
			}
		}

		k, v := existsBlockRecord(ns, 102)
		var rec blockRecord
		if err := readRawBlockRecord(k, v, &rec); err != nil {
			return err
		}
		want := []chainhash.Hash{received.TxHash()}
		if !reflect.DeepEqual(rec.transactions, want) {
			return fmt.Errorf("block 102 lists %v, want %v",
				rec.transactions, want)
		}
		if _, v := existsBlockRecord(ns, 103); v != nil {
			return errors.New("block 103 is still recorded")
		}

		var listed int
		err := s.RangeTransactions(ns, 0, -1, func(
			details []TxDetails) (bool, error) {

			listed += len(details)
			return false, nil
		})
		if err != nil {
			return err
		}
		if listed != 4 {
			return fmt.Errorf("%d transactions listed, want 4", listed)
		}

		minedBalance, err := fetchMinedBalance(ns)
		if err != nil {
			return err
		}
		if minedBalance != 7e7 {
			return fmt.Errorf("mined balance %v, want %v",
				minedBalance, bchutil.Amount(7e7))
		}
		return nil
	}

	applyMigration(
		t, beforeMigration, afterMigration, removeTxsOfOthers, false,
	)
}
