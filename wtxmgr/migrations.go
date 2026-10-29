package wtxmgr

import (
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchwallet/walletdb"
	"github.com/gcash/bchwallet/walletdb/migration"
)

// versions is a list of the different database versions. The last entry should
// reflect the latest database state. If the database happens to be at a version
// number lower than the latest, migrations will be performed in order to catch
// it up.
var versions = []migration.Version{
	{
		Number:    1,
		Migration: nil,
	},
	{
		Number:    2,
		Migration: dropTransactionHistory,
	},
	{
		Number:    3,
		Migration: markCreditsSpentByMinedTxs,
	},
	{
		Number:    4,
		Migration: removeTxsOfOthers,
	},
}

// getLatestVersion returns the version number of the latest database version.
func getLatestVersion() uint32 {
	return versions[len(versions)-1].Number
}

// MigrationManager is an implementation of the migration.Manager interface that
// will be used to handle migrations for the address manager. It exposes the
// necessary parameters required to successfully perform migrations.
type MigrationManager struct {
	ns walletdb.ReadWriteBucket
}

// A compile-time assertion to ensure that MigrationManager implements the
// migration.Manager interface.
var _ migration.Manager = (*MigrationManager)(nil)

// NewMigrationManager creates a new migration manager for the transaction
// manager. The given bucket should reflect the top-level bucket in which all
// of the transaction manager's data is contained within.
func NewMigrationManager(ns walletdb.ReadWriteBucket) *MigrationManager {
	return &MigrationManager{ns: ns}
}

// Name returns the name of the service we'll be attempting to upgrade.
//
// NOTE: This method is part of the migration.Manager interface.
func (m *MigrationManager) Name() string {
	return "wallet transaction manager"
}

// Namespace returns the top-level bucket of the service.
//
// NOTE: This method is part of the migration.Manager interface.
func (m *MigrationManager) Namespace() walletdb.ReadWriteBucket {
	return m.ns
}

// CurrentVersion returns the current version of the service's database.
//
// NOTE: This method is part of the migration.Manager interface.
func (m *MigrationManager) CurrentVersion(ns walletdb.ReadBucket) (uint32, error) {
	if ns == nil {
		ns = m.ns
	}
	return fetchVersion(ns)
}

// SetVersion sets the version of the service's database.
//
// NOTE: This method is part of the migration.Manager interface.
func (m *MigrationManager) SetVersion(ns walletdb.ReadWriteBucket,
	version uint32) error {

	if ns == nil {
		ns = m.ns
	}
	return putVersion(ns, version)
}

// Versions returns all of the available database versions of the service.
//
// NOTE: This method is part of the migration.Manager interface.
func (m *MigrationManager) Versions() []migration.Version {
	return versions
}

// dropTransactionHistory is a migration that attempts to recreate the
// transaction store with a clean state.
func dropTransactionHistory(ns walletdb.ReadWriteBucket) error {
	log.Info("Dropping wallet transaction history")

	// To drop the store's transaction history, we'll need to remove all of
	// the relevant descendant buckets and key/value pairs.
	if err := deleteBuckets(ns); err != nil {
		return err
	}
	if err := ns.Delete(rootMinedBalance); err != nil {
		return err
	}

	// With everything removed, we'll now recreate our buckets.
	if err := createBuckets(ns); err != nil {
		return err
	}

	// Finally, we'll insert a 0 value for our mined balance.
	return putMinedBalance(ns, 0)
}

// markCreditsSpentByMinedTxs is a migration that marks spent the credits that
// a mined transaction spends but that the store kept unspent, and removes the
// unmined transactions that spend them again, along with the transactions
// that spend their outputs. The wallet recorded the transactions of a block in
// block order, and the store marks a credit spent only when it records the
// spending transaction after the credit, so a transaction that the block
// listed before its parent, as blocks that order their transactions by txid
// can, left the parent's output unspent.
func markCreditsSpentByMinedTxs(ns walletdb.ReadWriteBucket) error {
	// spend is an input of a mined transaction that spends an unspent
	// credit.
	type spend struct {
		rec        *TxRecord
		block      Block
		index      uint32
		unspentKey []byte
	}

	// The transaction records are all read before the store is changed,
	// since a bucket can't be changed while it is iterated.
	var spends []spend
	err := ns.NestedReadBucket(bucketTxRecords).ForEach(func(k, v []byte) error {
		var block Block
		if err := readRawTxRecordBlock(k, &block); err != nil {
			return err
		}
		var txHash chainhash.Hash
		copy(txHash[:], k[:chainhash.HashSize])
		rec := new(TxRecord)
		if err := readRawTxRecord(&txHash, v, rec); err != nil {
			return err
		}
		for i, input := range rec.MsgTx.TxIn {
			unspentKey, credKey := existsUnspent(
				ns, &input.PreviousOutPoint,
			)
			if credKey == nil {
				continue
			}
			spends = append(spends, spend{
				rec:        rec,
				block:      block,
				index:      uint32(i),
				unspentKey: unspentKey,
			})
		}
		return nil
	})
	if err != nil || len(spends) == 0 {
		return err
	}

	minedBalance, err := fetchMinedBalance(ns)
	if err != nil {
		return err
	}

	// removeDoubleSpends uses no field of the store.
	var s Store
	for _, sp := range spends {
		credKey := existsRawUnspent(ns, sp.unspentKey)
		if credKey == nil {
			continue
		}

		log.Infof("Marking output %v spent by transaction %v mined in "+
			"block %d", sp.rec.MsgTx.TxIn[sp.index].PreviousOutPoint,
			sp.rec.Hash, sp.block.Height)

		spender := indexedIncidence{
			incidence: incidence{txHash: sp.rec.Hash, block: sp.block},
			index:     sp.index,
		}
		amt, err := spendCredit(ns, credKey, &spender)
		if err != nil {
			return err
		}
		err = putDebit(
			ns, &sp.rec.Hash, sp.index, amt, &sp.block, credKey,
		)
		if err != nil {
			return err
		}
		if err := deleteRawUnspent(ns, sp.unspentKey); err != nil {
			return err
		}
		minedBalance -= amt

		if err := s.removeDoubleSpends(ns, sp.rec); err != nil {
			return err
		}
	}

	return putMinedBalance(ns, minedBalance)
}

// removeTxsOfOthers is a migration that removes the transactions that neither
// pay nor spend the wallet, which it recorded because its chain backend
// reported them: once the wallet published a transaction, the backend also
// watched the addresses it paid and the outputs it paid them, and reported
// their owners' later transactions too. A mined one has neither credits nor
// debits, and an unmined one neither credits nor an input spending the
// wallet's outputs.
func removeTxsOfOthers(ns walletdb.ReadWriteBucket) error {
	// minedTx is a mined transaction record.
	type minedTx struct {
		hash  chainhash.Hash
		block Block
	}

	// The transaction records are all read before the store is changed,
	// since a bucket can't be changed while it is iterated.
	var mined []minedTx
	err := ns.NestedReadBucket(bucketTxRecords).ForEach(func(k, v []byte) error {
		credits := makeReadCreditIterator(ns, k)
		if credits.next() {
			return nil
		}
		if credits.err != nil {
			return credits.err
		}
		debits := makeReadDebitIterator(ns, k)
		if debits.next() {
			return nil
		}
		if debits.err != nil {
			return debits.err
		}

		var tx minedTx
		copy(tx.hash[:], k[:chainhash.HashSize])
		if err := readRawTxRecordBlock(k, &tx.block); err != nil {
			return err
		}
		mined = append(mined, tx)
		return nil
	})
	if err != nil {
		return err
	}

	// PreviousPkScripts and removeConflict use no field of the store.
	var s Store
	var unmined []*TxRecord
	err = ns.NestedReadBucket(bucketUnmined).ForEach(func(k, v []byte) error {
		rec := new(TxRecord)
		if err := readRawUnminedHash(k, &rec.Hash); err != nil {
			return err
		}
		if err := readRawTxRecord(&rec.Hash, v, rec); err != nil {
			return err
		}
		for i := range rec.MsgTx.TxOut {
			credKey := canonicalOutPoint(&rec.Hash, uint32(i))
			if existsRawUnminedCredit(ns, credKey) != nil {
				return nil
			}
		}
		spent, err := s.PreviousPkScripts(ns, rec, nil)
		if err != nil || len(spent) != 0 {
			return err
		}
		unmined = append(unmined, rec)
		return nil
	})
	if err != nil {
		return err
	}

	for _, tx := range mined {
		log.Infof("Removing transaction %v mined in block %d, which "+
			"neither pays nor spends the wallet", tx.hash,
			tx.block.Height)

		if err := deleteTxRecord(ns, &tx.hash, &tx.block); err != nil {
			return err
		}
		if err := removeBlockRecordTx(ns, &tx.block, &tx.hash); err != nil {
			return err
		}
	}
	for _, rec := range unmined {
		// A transaction that spends another one's output goes with it.
		if existsRawUnmined(ns, rec.Hash[:]) == nil {
			continue
		}

		log.Infof("Removing unmined transaction %v, which neither pays "+
			"nor spends the wallet", rec.Hash)

		if err := s.removeConflict(ns, rec); err != nil {
			return err
		}
	}
	return nil
}
