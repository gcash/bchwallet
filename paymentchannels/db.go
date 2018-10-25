package paymentchannels

import (
	"bytes"
	"encoding/gob"
	"github.com/gcash/bchd/bchec"
	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/walletdb"
	"github.com/libp2p/go-libp2p-peer"
)

var (
	paymentChannelBucket = []byte("paymentchannels")
	openChannelsBucket   = []byte("openchannels")
	closedChannelsBucket = []byte("closedchannels")
	transactionsBucket   = []byte("transactions")
)

func init() {
	gob.Register(bchec.KoblitzCurve{})
	gob.Register(bchutil.AddressScriptHash{})
}

// initDatabase will attempt to create all of the database bucks if they do not
// yet exist.
func initDatabase(db walletdb.DB) error {
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		wb, err := tx.CreateTopLevelBucket(paymentChannelBucket)
		if err != nil {
			return err
		}
		if _, err = wb.CreateBucketIfNotExists(openChannelsBucket); err != nil {
			return err
		}
		if _, err = wb.CreateBucketIfNotExists(closedChannelsBucket); err != nil {
			return err
		}
		if _, err = wb.CreateBucketIfNotExists(transactionsBucket); err != nil {
			return err
		}
		return nil
	})
	if err != nil && err != walletdb.ErrBucketExists {
		return err
	}
	return nil
}

// serializableChannel is a struct that gob is capable of serializing
type serializableChannel struct {
	ID                 chainhash.Hash
	State              ChannelState
	Inbound            bool
	AddressID          []byte
	RemotePeerID       peer.ID
	Delay              uint32
	FeePerByte         bchutil.Amount
	DustLimit          bchutil.Amount
	RemotePayoutScript []byte
	LocalPayoutScript  []byte
	RemoteBalance      bchutil.Amount
	LocalBalance       bchutil.Amount
	CommitmentTx       wire.MsgTx
	FundingTxid        chainhash.Hash
	FundingOutpoint    wire.OutPoint
	PayoutTxid         chainhash.Hash
	TransactionCount   uint64
	RedeemScript       []byte

	RemotePubkey             []byte
	LocalPrivkey             []byte
	RemoteRevocationPrivkeys map[string][]byte
	RemoteRevocationPubkey   []byte
	LocalRevocationPrivkey   []byte
	ChannelAddress           string
}

func serializeChannel(c Channel) ([]byte, error) {
	serializable := serializableChannel{
		ID:                       c.ID,
		State:                    c.State,
		Inbound:                  c.Inbound,
		AddressID:                c.AddressID,
		RemotePeerID:             c.RemotePeerID,
		Delay:                    c.Delay,
		FeePerByte:               c.FeePerByte,
		DustLimit:                c.DustLimit,
		RemotePayoutScript:       c.RemotePayoutScript,
		LocalPayoutScript:        c.LocalPayoutScript,
		RemoteBalance:            c.RemoteBalance,
		LocalBalance:             c.LocalBalance,
		CommitmentTx:             c.CommitmentTx,
		FundingTxid:              c.FundingTxid,
		FundingOutpoint:          c.FundingOutpoint,
		PayoutTxid:               c.PayoutTxid,
		TransactionCount:         c.TransactionCount,
		RedeemScript:             c.RedeemScript,
		RemotePubkey:             c.RemotePubkey.SerializeCompressed(),
		LocalPrivkey:             c.LocalPrivkey.Serialize(),
		RemoteRevocationPubkey:   c.RemoteRevocationPubkey.SerializeCompressed(),
		LocalRevocationPrivkey:   c.LocalRevocationPrivkey.Serialize(),
		ChannelAddress:           c.ChannelAddress.String(),
		RemoteRevocationPrivkeys: make(map[string][]byte),
	}
	for k, v := range c.RemoteRevocationPrivkeys {
		serializable.RemoteRevocationPrivkeys[k.String()] = v.Serialize()
	}
	var b bytes.Buffer
	encoder := gob.NewEncoder(&b)

	if err := encoder.Encode(serializable); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

func deserializeChannel(ser []byte, params *chaincfg.Params) (*Channel, error) {
	r := bytes.NewReader(ser)
	decoder := gob.NewDecoder(r)

	var serialable serializableChannel
	if err := decoder.Decode(&serialable); err != nil {
		return nil, err
	}
	c := Channel{
		ID:                       serialable.ID,
		State:                    serialable.State,
		Inbound:                  serialable.Inbound,
		AddressID:                serialable.AddressID,
		RemotePeerID:             serialable.RemotePeerID,
		Delay:                    serialable.Delay,
		FeePerByte:               serialable.FeePerByte,
		DustLimit:                serialable.DustLimit,
		RemotePayoutScript:       serialable.RemotePayoutScript,
		LocalPayoutScript:        serialable.LocalPayoutScript,
		RemoteBalance:            serialable.RemoteBalance,
		LocalBalance:             serialable.LocalBalance,
		CommitmentTx:             serialable.CommitmentTx,
		FundingTxid:              serialable.FundingTxid,
		FundingOutpoint:          serialable.FundingOutpoint,
		PayoutTxid:               serialable.PayoutTxid,
		TransactionCount:         serialable.TransactionCount,
		RedeemScript:             serialable.RedeemScript,
		RemoteRevocationPrivkeys: make(map[bchutil.Address]bchec.PrivateKey),
	}
	remotePubkey, err := bchec.ParsePubKey(serialable.RemotePubkey, bchec.S256())
	if err != nil {
		return nil, err
	}
	c.RemotePubkey = *remotePubkey

	localPrivkey, _ := bchec.PrivKeyFromBytes(bchec.S256(), serialable.LocalPrivkey)
	c.LocalPrivkey = *localPrivkey

	remoteRevocationPubkey, err := bchec.ParsePubKey(serialable.RemoteRevocationPubkey, bchec.S256())
	if err != nil {
		return nil, err
	}
	c.RemoteRevocationPubkey = *remoteRevocationPubkey

	localRevocationPrivkey, _ := bchec.PrivKeyFromBytes(bchec.S256(), serialable.LocalRevocationPrivkey)
	c.LocalRevocationPrivkey = *localRevocationPrivkey

	channelAddress, err := bchutil.DecodeAddress(serialable.ChannelAddress, params)
	if err != nil {
		return nil, err
	}
	c.ChannelAddress = channelAddress

	for k, v := range serialable.RemoteRevocationPrivkeys {
		privkey, _ := bchec.PrivKeyFromBytes(bchec.S256(), v)
		addr, err := bchutil.DecodeAddress(k, params)
		if err != nil {
			return nil, err
		}
		c.RemoteRevocationPrivkeys[addr] = *privkey
	}
	return &c, nil
}
