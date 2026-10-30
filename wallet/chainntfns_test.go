package wallet

import (
	"encoding/binary"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/chain"
	"github.com/gcash/bchwallet/waddrmgr"
	"github.com/gcash/bchwallet/walletdb"
	_ "github.com/gcash/bchwallet/walletdb/bdb"
	"github.com/gcash/bchwallet/wtxmgr"
)

const (
	// defaultBlockInterval is the default time interval between any two
	// blocks in a mocked chain.
	defaultBlockInterval = 10 * time.Minute
)

var (
	// chainParams are the chain parameters used throughout the wallet
	// tests.
	chainParams = chaincfg.MainNetParams
)

// mockChainConn is a mock in-memory implementation of the chainConn interface
// that will be used for the birthday block sanity check tests. The struct is
// capable of being backed by a chain in order to reproduce real-world
// scenarios.
type mockChainConn struct {
	chainTip    uint32
	blockHashes map[uint32]chainhash.Hash
	blocks      map[chainhash.Hash]*wire.MsgBlock
}

var _ chainConn = (*mockChainConn)(nil)

// createMockChainConn creates a new mock chain connection backed by a chain
// with N blocks. Each block has a timestamp that is exactly blockInterval after
// the previous block's timestamp.
func createMockChainConn(genesis *wire.MsgBlock, n uint32,
	blockInterval time.Duration) *mockChainConn {

	c := &mockChainConn{
		chainTip:    n,
		blockHashes: make(map[uint32]chainhash.Hash),
		blocks:      make(map[chainhash.Hash]*wire.MsgBlock),
	}

	genesisHash := genesis.BlockHash()
	c.blockHashes[0] = genesisHash
	c.blocks[genesisHash] = genesis

	for i := uint32(1); i <= n; i++ {
		prevTimestamp := c.blocks[c.blockHashes[i-1]].Header.Timestamp
		block := &wire.MsgBlock{
			Header: wire.BlockHeader{
				Timestamp: prevTimestamp.Add(blockInterval),
			},
		}

		blockHash := block.BlockHash()
		c.blockHashes[i] = blockHash
		c.blocks[blockHash] = block
	}

	return c
}

// GetBestBlock returns the hash and height of the best block known to the
// backend.
func (c *mockChainConn) GetBestBlock() (*chainhash.Hash, int32, error) {
	bestHash, ok := c.blockHashes[c.chainTip]
	if !ok {
		return nil, 0, fmt.Errorf("block with height %d not found",
			c.chainTip)
	}

	return &bestHash, int32(c.chainTip), nil
}

// GetBlockHash returns the hash of the block with the given height.
func (c *mockChainConn) GetBlockHash(height int64) (*chainhash.Hash, error) {
	hash, ok := c.blockHashes[uint32(height)]
	if !ok {
		return nil, fmt.Errorf("block with height %d not found", height)
	}

	return &hash, nil
}

// GetBlockHeader returns the header for the block with the given hash.
func (c *mockChainConn) GetBlockHeader(hash *chainhash.Hash) (*wire.BlockHeader, error) {
	block, ok := c.blocks[*hash]
	if !ok {
		return nil, fmt.Errorf("header for block %v not found", hash)
	}

	return &block.Header, nil
}

// mockBirthdayStore is a mock in-memory implementation of the birthdayStore interface
// that will be used for the birthday block sanity check tests.
type mockBirthdayStore struct {
	birthday              time.Time
	birthdayBlock         *waddrmgr.BlockStamp
	birthdayBlockVerified bool
	syncedTo              waddrmgr.BlockStamp
}

var _ birthdayStore = (*mockBirthdayStore)(nil)

// Birthday returns the birthday timestamp of the wallet.
func (s *mockBirthdayStore) Birthday() time.Time {
	return s.birthday
}

// BirthdayBlock returns the birthday block of the wallet.
func (s *mockBirthdayStore) BirthdayBlock() (waddrmgr.BlockStamp, bool, error) {
	if s.birthdayBlock == nil {
		err := waddrmgr.ManagerError{
			ErrorCode: waddrmgr.ErrBirthdayBlockNotSet,
		}
		return waddrmgr.BlockStamp{}, false, err
	}

	return *s.birthdayBlock, s.birthdayBlockVerified, nil
}

// SetBirthdayBlock updates the birthday block of the wallet to the given block.
// The boolean can be used to signal whether this block should be sanity checked
// the next time the wallet starts.
func (s *mockBirthdayStore) SetBirthdayBlock(block waddrmgr.BlockStamp) error {
	s.birthdayBlock = &block
	s.birthdayBlockVerified = true
	s.syncedTo = block
	return nil
}

// TestBirthdaySanityCheckEmptyBirthdayBlock ensures that a sanity check is not
// done if the birthday block does not exist in the first place.
func TestBirthdaySanityCheckEmptyBirthdayBlock(t *testing.T) {
	t.Parallel()

	chainConn := &mockChainConn{}

	// Our birthday store will reflect that we don't have a birthday block
	// set, so we should not attempt a sanity check.
	birthdayStore := &mockBirthdayStore{}

	birthdayBlock, err := birthdaySanityCheck(chainConn, birthdayStore)
	if !waddrmgr.IsError(err, waddrmgr.ErrBirthdayBlockNotSet) {
		t.Fatalf("expected ErrBirthdayBlockNotSet, got %v", err)
	}

	if birthdayBlock != nil {
		t.Fatalf("expected birthday block to be nil due to not being "+
			"set, got %v", *birthdayBlock)
	}
}

// TestBirthdaySanityCheckVerifiedBirthdayBlock ensures that a sanity check is
// not performed if the birthday block has already been verified.
func TestBirthdaySanityCheckVerifiedBirthdayBlock(t *testing.T) {
	t.Parallel()

	const chainTip = 5000
	chainConn := createMockChainConn(
		chainParams.GenesisBlock, chainTip, defaultBlockInterval,
	)
	expectedBirthdayBlock := waddrmgr.BlockStamp{Height: 1337}

	// Our birthday store reflects that our birthday block has already been
	// verified and should not require a sanity check.
	birthdayStore := &mockBirthdayStore{
		birthdayBlock:         &expectedBirthdayBlock,
		birthdayBlockVerified: true,
		syncedTo: waddrmgr.BlockStamp{
			Height: chainTip,
		},
	}

	// Now, we'll run the sanity check. We should see that the birthday
	// block hasn't changed.
	birthdayBlock, err := birthdaySanityCheck(chainConn, birthdayStore)
	if err != nil {
		t.Fatalf("unable to sanity check birthday block: %v", err)
	}
	if !reflect.DeepEqual(*birthdayBlock, expectedBirthdayBlock) {
		t.Fatalf("expected birthday block %v, got %v",
			expectedBirthdayBlock, birthdayBlock)
	}

	// To ensure the sanity check didn't proceed, we'll check our synced to
	// height, as this value should have been modified if a new candidate
	// was found.
	if birthdayStore.syncedTo.Height != chainTip {
		t.Fatalf("expected synced height remain the same (%d), got %d",
			chainTip, birthdayStore.syncedTo.Height)
	}
}

// TestBirthdaySanityCheckLowerEstimate ensures that we can properly locate a
// better birthday block candidate if our estimate happens to be too far back in
// the chain.
func TestBirthdaySanityCheckLowerEstimate(t *testing.T) {
	t.Parallel()

	// We'll start by defining our birthday timestamp to be around the
	// timestamp of the 1337th block.
	genesisTimestamp := chainParams.GenesisBlock.Header.Timestamp
	birthday := genesisTimestamp.Add(1337 * defaultBlockInterval)

	// We'll establish a connection to a mock chain of 5000 blocks.
	chainConn := createMockChainConn(
		chainParams.GenesisBlock, 5000, defaultBlockInterval,
	)

	// Our birthday store will reflect that our birthday block is currently
	// set as the genesis block. This value is too low and should be
	// adjusted by the sanity check.
	birthdayStore := &mockBirthdayStore{
		birthday: birthday,
		birthdayBlock: &waddrmgr.BlockStamp{
			Hash:      *chainParams.GenesisHash,
			Height:    0,
			Timestamp: genesisTimestamp,
		},
		birthdayBlockVerified: false,
		syncedTo: waddrmgr.BlockStamp{
			Height: 5000,
		},
	}

	// We'll perform the sanity check and determine whether we were able to
	// find a better birthday block candidate.
	birthdayBlock, err := birthdaySanityCheck(chainConn, birthdayStore)
	if err != nil {
		t.Fatalf("unable to sanity check birthday block: %v", err)
	}
	if birthday.Sub(birthdayBlock.Timestamp) >= birthdayBlockDelta {
		t.Fatalf("expected birthday block timestamp=%v to be within "+
			"%v of birthday timestamp=%v", birthdayBlock.Timestamp,
			birthdayBlockDelta, birthday)
	}

	// Finally, our synced to height should now reflect our new birthday
	// block to ensure the wallet doesn't miss any events from this point
	// forward.
	if !reflect.DeepEqual(birthdayStore.syncedTo, *birthdayBlock) {
		t.Fatalf("expected syncedTo and birthday block to match: "+
			"%v vs %v", birthdayStore.syncedTo, birthdayBlock)
	}
}

// TestBirthdaySanityCheckHigherEstimate ensures that we can properly locate a
// better birthday block candidate if our estimate happens to be too far in the
// chain.
func TestBirthdaySanityCheckHigherEstimate(t *testing.T) {
	t.Parallel()

	// We'll start by defining our birthday timestamp to be around the
	// timestamp of the 1337th block.
	genesisTimestamp := chainParams.GenesisBlock.Header.Timestamp
	birthday := genesisTimestamp.Add(1337 * defaultBlockInterval)

	// We'll establish a connection to a mock chain of 5000 blocks.
	chainConn := createMockChainConn(
		chainParams.GenesisBlock, 5000, defaultBlockInterval,
	)

	// Our birthday store will reflect that our birthday block is currently
	// set as the chain tip. This value is too high and should be adjusted
	// by the sanity check.
	bestBlock := chainConn.blocks[chainConn.blockHashes[5000]]
	birthdayStore := &mockBirthdayStore{
		birthday: birthday,
		birthdayBlock: &waddrmgr.BlockStamp{
			Hash:      bestBlock.BlockHash(),
			Height:    5000,
			Timestamp: bestBlock.Header.Timestamp,
		},
		birthdayBlockVerified: false,
		syncedTo: waddrmgr.BlockStamp{
			Height: 5000,
		},
	}

	// We'll perform the sanity check and determine whether we were able to
	// find a better birthday block candidate.
	birthdayBlock, err := birthdaySanityCheck(chainConn, birthdayStore)
	if err != nil {
		t.Fatalf("unable to sanity check birthday block: %v", err)
	}
	if birthday.Sub(birthdayBlock.Timestamp) >= birthdayBlockDelta {
		t.Fatalf("expected birthday block timestamp=%v to be within "+
			"%v of birthday timestamp=%v", birthdayBlock.Timestamp,
			birthdayBlockDelta, birthday)
	}

	// Finally, our synced to height should now reflect our new birthday
	// block to ensure the wallet doesn't miss any events from this point
	// forward.
	if !reflect.DeepEqual(birthdayStore.syncedTo, *birthdayBlock) {
		t.Fatalf("expected syncedTo and birthday block to match: "+
			"%v vs %v", birthdayStore.syncedTo, birthdayBlock)
	}
}

// txRecord returns a record of tx, as a chain backend makes one.
func txRecord(t *testing.T, tx *wire.MsgTx) *wtxmgr.TxRecord {
	t.Helper()

	rec, err := wtxmgr.NewTxRecordFromMsgTx(tx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// TestSortParentsFirst checks that a chain of transactions listed child first
// is sorted parents first, and that the other transactions keep their order.
func TestSortParentsFirst(t *testing.T) {
	a := spendTx([]wire.OutPoint{{Hash: chainhash.Hash{1}}}, &wire.TxOut{})
	b := spendTx([]wire.OutPoint{{Hash: a.TxHash()}}, &wire.TxOut{})
	c := spendTx([]wire.OutPoint{{Hash: b.TxHash()}}, &wire.TxOut{})
	x := spendTx([]wire.OutPoint{{Hash: chainhash.Hash{2}}}, &wire.TxOut{})

	var recs []*wtxmgr.TxRecord
	for _, tx := range []*wire.MsgTx{c, x, b, a} {
		recs = append(recs, txRecord(t, tx))
	}

	var got []chainhash.Hash
	for _, rec := range sortParentsFirst(recs) {
		got = append(got, rec.Hash)
	}
	want := []chainhash.Hash{a.TxHash(), b.TxHash(), c.TxHash(), x.TxHash()}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sorted %v, want %v", got, want)
	}
}

// TestAddBlockTxsSpendBeforeParent checks that a transaction that spends an
// output of another in the same block marks the output spent when the block
// lists it first, as blocks that order their transactions by txid can.
func TestAddBlockTxsSpendBeforeParent(t *testing.T) {
	tests := []struct {
		name string

		// unmined is whether the wallet records the transactions as
		// unmined before the block, as it does those it sends.
		unmined bool
	}{
		{name: "sent by the wallet", unmined: true},
		{name: "first seen in the block", unmined: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w := newRegtestWallet(t)
			script := walletScript(t, w)

			funding := addCredit(t, w, &wire.TxOut{
				Value: 100000, PkScript: script,
			})
			parent := spendTx([]wire.OutPoint{funding},
				&wire.TxOut{Value: 10000, PkScript: script},
				&wire.TxOut{Value: 89000, PkScript: script})
			spend := spendTx(
				[]wire.OutPoint{{Hash: parent.TxHash(), Index: 1}},
				&wire.TxOut{Value: 88000, PkScript: script})
			parentRec, spendRec := txRecord(t, parent), txRecord(t, spend)

			if test.unmined {
				err := walletdb.Update(w.db, func(
					dbtx walletdb.ReadWriteTx) error {

					err := w.addRelevantTx(dbtx, parentRec, nil)
					if err != nil {
						return err
					}
					return w.addRelevantTx(dbtx, spendRec, nil)
				})
				if err != nil {
					t.Fatal(err)
				}
			}

			block := &wtxmgr.BlockMeta{
				Block: wtxmgr.Block{Hash: chainhash.Hash{2}, Height: 2},
				Time:  testBlock.Time.Add(defaultBlockInterval),
			}
			err := walletdb.Update(w.db, func(
				dbtx walletdb.ReadWriteTx) error {

				return w.addBlockTxs(dbtx, []*wtxmgr.TxRecord{
					spendRec, parentRec,
				}, block)
			})
			if err != nil {
				t.Fatal(err)
			}

			var (
				credits []wtxmgr.Credit
				unmined []*wire.MsgTx
				balance bchutil.Amount
			)
			err = walletdb.View(w.db, func(dbtx walletdb.ReadTx) error {
				ns := dbtx.ReadBucket(wtxmgrNamespaceKey)
				var err error
				credits, err = w.TxStore.UnspentOutputs(ns)
				if err != nil {
					return err
				}
				unmined, err = w.TxStore.UnminedTxs(ns)
				if err != nil {
					return err
				}
				balance, err = w.TxStore.Balance(ns, 1, block.Height)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}

			want := map[wire.OutPoint]bool{
				{Hash: parent.TxHash(), Index: 0}: true,
				{Hash: spend.TxHash(), Index: 0}:  true,
			}
			got := make(map[wire.OutPoint]bool)
			for _, credit := range credits {
				got[credit.OutPoint] = true
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("unspent outputs %v, want %v", got, want)
			}
			if len(unmined) != 0 {
				t.Errorf("%d transactions left unmined", len(unmined))
			}
			if balance != 98000 {
				t.Errorf("balance %v, want %v", balance,
					bchutil.Amount(98000))
			}
		})
	}
}

// TestAddNotifiedTxWalletOnly checks that of the transactions a chain backend
// reports, the wallet records its own, which pay it, spend its outputs or
// were published by it, and not the spend of an output it paid someone, which
// a backend reports while it watches the outputs of a published transaction.
func TestAddNotifiedTxWalletOnly(t *testing.T) {
	w := newRegtestWallet(t)
	script := walletScript(t, w)
	someone, err := bchutil.NewAddressPubKeyHash(
		bchutil.Hash160([]byte("someone else")), w.chainParams)
	if err != nil {
		t.Fatal(err)
	}
	otherScript, err := txscript.PayToAddrScript(someone)
	if err != nil {
		t.Fatal(err)
	}

	funding := addCredit(t, w, &wire.TxOut{Value: 100000, PkScript: script})
	payment := spendTx([]wire.OutPoint{funding},
		&wire.TxOut{Value: 10000, PkScript: otherScript},
		&wire.TxOut{Value: 89000, PkScript: script})
	paymentSpend := spendTx(
		[]wire.OutPoint{{Hash: payment.TxHash(), Index: 0}},
		&wire.TxOut{Value: 9000, PkScript: otherScript})
	changeSpend := spendTx(
		[]wire.OutPoint{{Hash: payment.TxHash(), Index: 1}},
		&wire.TxOut{Value: 88000, PkScript: otherScript})
	received := spendTx([]wire.OutPoint{{Hash: chainhash.Hash{7}}},
		&wire.TxOut{Value: 5000, PkScript: script})
	published := spendTx([]wire.OutPoint{{Hash: chainhash.Hash{8}}},
		&wire.TxOut{Value: 7000, PkScript: otherScript})
	unminedSpend := spendTx(
		[]wire.OutPoint{{Hash: paymentSpend.TxHash(), Index: 0}},
		&wire.TxOut{Value: 8000, PkScript: otherScript})

	// The wallet records the transactions it publishes, its own or not.
	err = walletdb.Update(w.db, func(dbtx walletdb.ReadWriteTx) error {
		err := w.addRelevantTx(dbtx, txRecord(t, payment), nil)
		if err != nil {
			return err
		}
		return w.addRelevantTx(dbtx, txRecord(t, published), nil)
	})
	if err != nil {
		t.Fatal(err)
	}

	block := &wtxmgr.BlockMeta{
		Block: wtxmgr.Block{Hash: chainhash.Hash{2}, Height: 2},
		Time:  testBlock.Time.Add(defaultBlockInterval),
	}
	var recs []*wtxmgr.TxRecord
	for _, tx := range []*wire.MsgTx{paymentSpend, changeSpend, received,
		published, payment} {

		recs = append(recs, txRecord(t, tx))
	}
	err = walletdb.Update(w.db, func(dbtx walletdb.ReadWriteTx) error {
		if err := w.addBlockTxs(dbtx, recs, block); err != nil {
			return err
		}

		// An unmined transaction, as a bchd backend reports one.
		return w.addNotifiedTx(dbtx, txRecord(t, unminedSpend), nil)
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		tx   *wire.MsgTx
		want bool
	}{
		{"the payment", payment, true},
		{"the spend of the payment", paymentSpend, false},
		{"the spend of the change", changeSpend, true},
		{"a transaction paying the wallet", received, true},
		{"a published transaction", published, true},
		{"an unmined spend of the payment's spend", unminedSpend, false},
	}
	for _, test := range tests {
		if got := recorded(t, w, test.tx); got != test.want {
			t.Errorf("%s recorded: %v, want %v", test.name, got,
				test.want)
		}
	}
}

// TestActiveDataWatchesUnminedSpends checks that the outputs a rescan watches
// include one that an unmined payment of the wallet spends, so that the backend
// reports the block that confirms the payment also when it pays none of the
// wallet's addresses, as a payment of all of the wallet's coins doesn't.
func TestActiveDataWatchesUnminedSpends(t *testing.T) {
	w := newRegtestWallet(t)
	script := walletScript(t, w)
	someone, err := bchutil.NewAddressPubKeyHash(
		bchutil.Hash160([]byte("someone else")), w.chainParams)
	if err != nil {
		t.Fatal(err)
	}
	otherScript, err := txscript.PayToAddrScript(someone)
	if err != nil {
		t.Fatal(err)
	}

	kept := addCredit(t, w, &wire.TxOut{Value: 50000, PkScript: script})
	spent := addCredit(t, w, &wire.TxOut{Value: 100000, PkScript: script})
	payment := spendTx([]wire.OutPoint{spent},
		&wire.TxOut{Value: 99000, PkScript: otherScript})
	insertTx(t, w, payment, nil)

	var credits []wtxmgr.Credit
	err = walletdb.View(w.db, func(dbtx walletdb.ReadTx) error {
		var err error
		_, credits, err = w.activeData(dbtx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	watched := make(map[wire.OutPoint]bool)
	for _, credit := range credits {
		watched[credit.OutPoint] = true
	}
	if !watched[spent] {
		t.Error("the output the unmined payment spends isn't watched")
	}
	if !watched[kept] {
		t.Error("the unspent output isn't watched")
	}
}

// filterRecordingChainClient is a mock chain client whose best block is the
// one after the wallet's tip, and which records the outpoints that blocks are
// filtered for.
type filterRecordingChainClient struct {
	mockChainClient
	bestHeight int32
	watched    map[wire.OutPoint]bchutil.Address
}

func (c *filterRecordingChainClient) GetBestBlock() (*chainhash.Hash, int32, error) {
	return &chainhash.Hash{2}, c.bestHeight, nil
}

func (c *filterRecordingChainClient) GetBlockHash(int64) (*chainhash.Hash, error) {
	return &chainhash.Hash{2}, nil
}

func (c *filterRecordingChainClient) GetBlockHeader(*chainhash.Hash) (
	*wire.BlockHeader, error) {

	return &wire.BlockHeader{Timestamp: time.Unix(1700000000, 0)}, nil
}

func (c *filterRecordingChainClient) FilterBlocks(
	req *chain.FilterBlocksRequest) (*chain.FilterBlocksResponse, error) {

	c.watched = req.WatchedOutPoints
	return nil, nil
}

// TestRecoveryWatchesUnminedSpends checks that the scan for used addresses,
// which a wallet with a recovery window runs at every start over the blocks
// mined since it last synced, watches an output that an unmined payment of the
// wallet spends, so that it finds a block that confirmed the payment while the
// wallet was stopped, which the rescan after it doesn't scan again.
func TestRecoveryWatchesUnminedSpends(t *testing.T) {
	w := newRegtestWallet(t)
	script := walletScript(t, w)
	someone, err := bchutil.NewAddressPubKeyHash(
		bchutil.Hash160([]byte("someone else")), w.chainParams)
	if err != nil {
		t.Fatal(err)
	}
	otherScript, err := txscript.PayToAddrScript(someone)
	if err != nil {
		t.Fatal(err)
	}

	kept := addCredit(t, w, &wire.TxOut{Value: 50000, PkScript: script})
	spent := addCredit(t, w, &wire.TxOut{Value: 100000, PkScript: script})
	payment := spendTx([]wire.OutPoint{spent},
		&wire.TxOut{Value: 99000, PkScript: otherScript})
	insertTx(t, w, payment, nil)

	// The scan reports its progress on rescanNotifications.
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-w.rescanNotifications:
			case <-done:
				return
			}
		}
	}()

	client := &filterRecordingChainClient{
		bestHeight: w.Manager.SyncedTo().Height + 1,
	}
	birthday := waddrmgr.BlockStamp{}
	if err := w.recovery(client, &birthday); err != nil {
		t.Fatal(err)
	}
	if client.watched == nil {
		t.Fatal("the scan filtered no blocks")
	}
	if _, ok := client.watched[spent]; !ok {
		t.Error("the output the unmined payment spends isn't watched")
	}
	if _, ok := client.watched[kept]; !ok {
		t.Error("the unspent output isn't watched")
	}
}

// notifyWatchingChainClient is a mock chain client whose notifications a
// test sends, and which records the addresses that the wallet has it watch.
type notifyWatchingChainClient struct {
	notifyingChainClient
	mu      sync.Mutex
	watched map[string]bool
}

func (c *notifyWatchingChainClient) NotifyReceived(addrs []bchutil.Address) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, addr := range addrs {
		c.watched[addr.EncodeAddress()] = true
	}
	return nil
}

func (c *notifyWatchingChainClient) isWatched(addr bchutil.Address) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.watched[addr.EncodeAddress()]
}

// TestNewAddressesWatched checks that the backend watches the addresses that
// the wallet derives as payments use the ones before them, as well as those
// that the rescan watches when the wallet starts, so that a payment to any
// address that CurrentAddress hands out is seen. CurrentAddress hands out the
// unused address whose hash sorts first, which may be one the wallet started
// with or one it derived since.
func TestNewAddressesWatched(t *testing.T) {
	w := newRegtestWallet(t)
	client := &notifyWatchingChainClient{
		notifyingChainClient: notifyingChainClient{
			ntfns: make(chan interface{}),
		},
		watched: make(map[string]bool),
	}
	w.chainClient = client

	// The rescan that starts with the wallet watches the addresses it has.
	err := walletdb.View(w.db, func(dbtx walletdb.ReadTx) error {
		addrs, _, err := w.activeData(dbtx)
		if err != nil {
			return err
		}
		return client.NotifyReceived(addrs)
	})
	if err != nil {
		t.Fatal(err)
	}

	// The recovery's interrupt handler takes what interruptRecovery sends.
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-w.recoveryInterruptChan:
			case <-done:
				return
			}
		}
	}()
	w.wg.Add(1)
	go w.handleChainNotifications()
	defer func() {
		w.Stop()
		waitFor(t, "the chain notification handler", w.wg.Wait)
	}()

	someone, err := bchutil.NewAddressPubKeyHash(
		bchutil.Hash160([]byte("someone else")), w.chainParams)
	if err != nil {
		t.Fatal(err)
	}
	otherScript, err := txscript.PayToAddrScript(someone)
	if err != nil {
		t.Fatal(err)
	}
	record := func(i int, pkScript []byte) *wtxmgr.TxRecord {
		tx := &wire.MsgTx{
			Version: wire.TxVersion,
			TxIn: []*wire.TxIn{{PreviousOutPoint: wire.OutPoint{
				Hash: chainhash.Hash{byte(i)},
			}}},
			TxOut: []*wire.TxOut{{Value: 10000, PkScript: pkScript}},
		}
		rec, err := wtxmgr.NewTxRecordFromMsgTx(tx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	// handled returns once the handler took a transaction that doesn't
	// concern the wallet, and so has handled the notifications before it.
	unrelated := record(0, otherScript)
	handled := func() {
		client.ntfns <- chain.RelevantTx{TxRecord: unrelated}
	}

	for i := 1; i <= 10; i++ {
		addr, err := w.CurrentAddress(0, waddrmgr.KeyScopeBIP0044)
		if err != nil {
			t.Fatal(err)
		}
		script, err := txscript.PayToAddrScript(addr)
		if err != nil {
			t.Fatal(err)
		}
		client.ntfns <- chain.RelevantTx{TxRecord: record(i, script)}
		handled()
	}

	err = walletdb.View(w.db, func(dbtx walletdb.ReadTx) error {
		ns := dbtx.ReadBucket(waddrmgrNamespaceKey)
		return w.Manager.ForEachActiveAddress(ns, func(addr bchutil.Address) error {
			if !client.isWatched(addr) {
				t.Errorf("the backend doesn't watch %v, which the wallet "+
					"derived after ten payments", addr)
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// interruptingChainClient is a mock chain client with a chain of distinct
// blocks. It interrupts the first filter request after the request's second
// block, as a client does when a relevant transaction arrives during the scan,
// and records the heights of the blocks that each request asks to scan.
type interruptingChainClient struct {
	mockChainClient
	bestHeight int32
	requests   [][]int32
}

// recoveryBlockHash returns the hash of the mock chain's block at height.
func recoveryBlockHash(height int32) *chainhash.Hash {
	var hash chainhash.Hash
	binary.BigEndian.PutUint32(hash[:], uint32(height))
	hash[31] = 0x11
	return &hash
}

func (c *interruptingChainClient) GetBestBlock() (*chainhash.Hash, int32,
	error) {

	return recoveryBlockHash(c.bestHeight), c.bestHeight, nil
}

func (c *interruptingChainClient) GetBlockHash(height int64) (
	*chainhash.Hash, error) {

	return recoveryBlockHash(int32(height)), nil
}

func (c *interruptingChainClient) GetBlockHeader(*chainhash.Hash) (
	*wire.BlockHeader, error) {

	return &wire.BlockHeader{Timestamp: time.Unix(1700000000, 0)}, nil
}

func (c *interruptingChainClient) FilterBlocks(
	req *chain.FilterBlocksRequest) (*chain.FilterBlocksResponse, error) {

	heights := make([]int32, 0, len(req.Blocks))
	for _, block := range req.Blocks {
		heights = append(heights, block.Height)
	}
	c.requests = append(c.requests, heights)

	if len(c.requests) == 1 {
		return &chain.FilterBlocksResponse{BatchIndex: 1},
			chain.ErrFilterReqInterrupt
	}
	return nil, nil
}

// TestRecoveryResumesAfterInterrupt checks that the scan for used addresses,
// interrupted part way through a batch as a relevant transaction does, starts
// again after the last block it finished and ends synced to the best block. It
// took the index of that block in the batch for its height, which matched no
// block of the scan, and then dereferenced the block it didn't find.
func TestRecoveryResumesAfterInterrupt(t *testing.T) {
	w := newRegtestWallet(t)

	// Start the scan well above the index of any block in its batch, as
	// on a real chain.
	const syncedHeight = 1000
	err := walletdb.Update(w.db, func(tx walletdb.ReadWriteTx) error {
		ns := tx.ReadWriteBucket(waddrmgrNamespaceKey)
		start := w.Manager.SyncedTo().Height + 1
		for height := start; height <= syncedHeight; height++ {
			err := w.Manager.SetSyncedTo(ns, &waddrmgr.BlockStamp{
				Height: height,
				Hash:   *recoveryBlockHash(height),
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The scan reports its progress on rescanNotifications.
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-w.rescanNotifications:
			case <-done:
				return
			}
		}
	}()

	client := &interruptingChainClient{bestHeight: syncedHeight + 6}
	birthday := waddrmgr.BlockStamp{}
	if err := w.recovery(client, &birthday); err != nil {
		t.Fatal(err)
	}

	want := [][]int32{
		{1001, 1002, 1003, 1004, 1005, 1006},
		{1003, 1004, 1005, 1006},
	}
	if !reflect.DeepEqual(client.requests, want) {
		t.Fatalf("the scan asked to filter blocks %v, want %v",
			client.requests, want)
	}
	synced := w.Manager.SyncedTo()
	if synced.Height != syncedHeight+6 ||
		synced.Hash != *recoveryBlockHash(syncedHeight + 6) {

		t.Fatalf("the wallet is synced to %d (%v), want %d",
			synced.Height, synced.Hash, syncedHeight+6)
	}
}
