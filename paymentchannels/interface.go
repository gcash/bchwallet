package paymentchannels

import (
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/waddrmgr"
	"github.com/gcash/bchwallet/wallet/txauthor"
)

// WalletBackend is an interface that defines a couple wallet functions that we will need
// for the payment channels. The reason we do this is just to avoid a circular import
// where we import the wallet package which imports the paymentchannels package.
// Instead we'll just pass the wallet into the NodeConfig as an implementation of the
// this interface.
type WalletBackend interface {
	// NewAddress returns the next external chained address for a wallet.
	NewAddress(account uint32, scope waddrmgr.KeyScope) (bchutil.Address, error)

	// CreateSimpleTx creates a new signed transaction spending unspent P2PKH
	// outputs with at least minconf confirmations spending to any number of
	// address/amount pairs.  Change and an appropriate transaction fee are
	// automatically included, if necessary.  All transaction creation through this
	// function is serialized to prevent the creation of many transactions which
	// spend the same outputs.
	CreateSimpleTx(account uint32, outputs []*wire.TxOut, minconf int32, satPerKb bchutil.Amount) (*txauthor.AuthoredTx, error)

	// PublishTransaction sends the transaction to the consensus RPC server so it
	// can be propagated to other nodes and eventually mined.
	//
	// This function is unstable and will be removed once syncing code is moved out
	// of the wallet.
	PublishTransaction(tx *wire.MsgTx) error

	// LockOutpoint marks an outpoint as locked, that is, it should not be used as
	// an input for newly created transactions.
	LockOutpoint(op wire.OutPoint)

	// UnlockOutpoint marks an outpoint as unlocked, that is, it may be used as an
	// input for newly created transactions.
	UnlockOutpoint(op wire.OutPoint)
}
