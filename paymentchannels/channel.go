package paymentchannels

import (
	"github.com/gcash/bchd/bchec"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/libp2p/go-libp2p-peer"
	"time"
)

type ChannelState uint8

const (
	// ChannelStateOpening is the initial state of the channel until we connect
	// and exchange commitment transactions.
	ChannelStateOpening ChannelState = 0

	// ChannelStateOpen is the normal running state for a channel.
	ChannelStateOpen    ChannelState = 1

	// ChannelStatePendingClosure is set when either party broadcasts a commitment
	// transaction. While the transaction is still unconfirmed it will be in this
	// state.
	ChannelStatePendingClosure ChannelState = 2

	// ChannelStateClosed represents a closed channel. This includes channels
	// closed by broadcasting a commitment transaction.
	ChannelStateClosed  ChannelState = 3

	// ChannelStateError means we messed up somehow.
	ChannelStateError ChannelState = 4
)

// String is the a stringer for ChannelState
func (s ChannelState) String() string {
	switch s {
	case ChannelStateOpening:
		return "Opening"
	case ChannelStateOpen:
		return "Open"
	case ChannelStatePendingClosure:
		return "Pending Closure"
	case ChannelStateClosed:
		return "Closed"
	case ChannelStateError:
		return "Error"
	default:
		return "Unknown"
	}
}

// Channel holds all the data relevant to a payment channel
type Channel struct {
	// ChannelID is the ID of the channel
	// TODO: how do we want to create this? We would have the sender set it to a random ID,
	// but then the recipient needs to reject channel open requests if he has the same ID in the
	// db. Alternatively we could set it to the funding txid, but we don't know it initially.
	ChannelID chainhash.Hash

	// State allows us to quickly tell what state the channel is in.
	State ChannelState

	// Incoming specifies whether the channel was opened by us or them
	Incoming bool

	// AddressID is taken from the cashaddr. It can be used by software to map channels
	// to external actions (like an order on a website).
	AddressID []byte

	// RemotePeerID is their libp2p peerID which we will use for communications.
	RemotePeerID peer.ID

	// RemotePubkey is the other party's public key which will be used in two spots:
	// 1. As part of the 2 of 2 multisig P2SH address that holds the channel funds and,
	// 2. As part of the 2 of 2 multisig P2SH address on the breach remedy output of our
	// commitment transactions.
	RemotePubkey *bchec.PublicKey

	// LocalPrivkey is used the same way as RemotePubkey except it's our key and we give the
	// corresponding pubkey to the other party.
	LocalPrivkey *bchec.PrivateKey

	// RemoteRevocationPrivkey represents the most recent revocation key we have for the
	// other party. Every time we update the channel state both parties not only
	// sign new commitment transactions, but they exchange their revocation private
	// keys which the other party can use to claim the funds if an old commitment
	// transaction gets broadcasted. This will be nil when the channel is first opened, but
	// will be updated once we make our first transaction.
	RemoteRevocationPrivkey *bchec.PrivateKey

	// LocalRevocationPrivkey is our revocation key that we will share with the other party
	// after each transaction. This will be updated with a new key after the old key is
	// disclosed.
	LocalRevocationPrivkey *bchec.PrivateKey

	// Delay is the negotiated timeout on commitment transactions. If a channel is
	// unilaterally closed the party which closed the channel will need to wait the delay.
	Delay time.Duration

	// FeePerByte is the fee rate used when calculating the fee on the payout transaction.
	// The fee should be subtracted evenly from the payout amount of both parties.
	FeePerByte bchutil.Amount

	// DustLimit is the minimum value of a TxOut used for a payout transaction. If the value
	// of an output is less than the dust limit then the output should be omitted from the
	// payout transaction.
	DustLimit bchutil.Amount

	// RemoteTxOut is an output for the other party which will be added to the channel payout
	// transaction. The script is the destination where the other party wants the funds to
	// go and the value is amount to be paid out. The amount will change with each new
	// channel update.
	RemoteTxOut wire.TxOut

	// LocalTxOut is the same as RemoteTxOut except it represents our TxOut.
	LocalTxOut wire.TxOut

	// CommitmentTx is out current commitment transaction. This should have the other
	// party's signature on their input. We just need to sign and broadcast if we
	// want to force close.
	CommitmentTx *wire.MsgTx

	// FundingTxid is the transaction ID of the transaction which funded the channel.
	FundingTxid chainhash.Hash

	// PayoutTxid is the transaction ID of the transaction which closed out the channel.
	PayoutTxid chainhash.Hash

	// TransactionCount is the total number of transactions (not including the initial funding)
	// that have been processed while the channel is open.
	TransactionCount uint64
}
