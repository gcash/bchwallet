package paymentchannels

import (
	"context"
	"fmt"
	"github.com/gcash/bchd/bchec"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/paymentchannels/pb"
	"github.com/gcash/bchwallet/walletdb"
	"github.com/go-errors/errors"
	ggio "github.com/gogo/protobuf/io"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	inet "github.com/libp2p/go-libp2p-net"
	"github.com/libp2p/go-libp2p-peer"
	"github.com/libp2p/go-libp2p-protocol"
	"time"
)

const (
	// ProtocolDHT defines the protocol ID for the DHT. We are prefixing it
	// with /bitcoincash/ to avoid the DHT accidentally merging with other
	// libp2p DHTs.
	ProtocolDHT = protocol.ID("/bitcoincash/kad/1.0.0")

	// ProtocolPaymentChannel is the protocol ID for the payment channel protocol.
	// It will be multiplexed along with the DHT protocol and messages on this
	// protocol will be routed to our handler.
	ProtocolPaymnetChannel = protocol.ID("/bitcoincash/paymentchannel/1.0.0")

	// DefaultNetworkTimeout is the time to wait for a response from the remote
	// peer before erroring.
	DefaultNetworkTimeout = time.Second * 10
)

// handlingNewStream handles new incoming streams from other peers.
// We use one stream per channel action (Open, Update, Close).
func (node *PaymentChannelNode) handleNewStream(s inet.Stream) {
	reader := ggio.NewDelimitedReader(s, inet.MessageSizeMax)
	var m pb.Message
	err := reader.ReadMsg(&m)
	if err != nil {
		log.Errorf("Error handling incoming stream from %s: %s", s.Conn().RemotePeer().Pretty(), err.Error())
		return
	}
	switch m.MessageType {
	case pb.Message_CHANNEL_OPEN:
		err = node.handleOpenChannelMessage(&m, s)
	default:
		log.Error("Received invalid incoming message type %s from %s:", m.MessageType.String(), s.Conn().RemotePeer().Pretty())
		return
	}
	if err != nil {
		log.Errorf("Error handling %s message: %s", m.MessageType.String(), err.Error())
	}
}

// openStream returns a new stream to a remote peer or an error if a connection could not be made.
func (node *PaymentChannelNode) openStream(ctx context.Context, peerID peer.ID) (inet.Stream, error) {
	return node.Host.NewStream(ctx, peerID, ProtocolPaymnetChannel)
}

func wrapMessage(message proto.Message, messageType pb.Message_MessageType) (proto.Message, error) {
	payload, err := ptypes.MarshalAny(message)
	if err != nil {
		return nil, err
	}
	m := pb.Message{
		MessageType: messageType,
		Payload:     payload,
	}
	return &m, nil
}

func readMessageWithTimeout(stream inet.Stream, timeout time.Duration) (*pb.Message, error) {
	reader := ggio.NewDelimitedReader(stream, inet.MessageSizeMax)
	var message pb.Message
	var err error

	resp := make(chan struct{})
	ticker := time.NewTicker(timeout)

	go func() {
		err = reader.ReadMsg(&message)
		resp <- struct{}{}
	}()
	select {
	case <-resp:
		return &message, err
	case <-ticker.C:
		return nil, errors.New("timed out waiting for remote peer's response")
	}
}

func (node *PaymentChannelNode) handleOpenChannelMessage(message *pb.Message, stream inet.Stream) error {
	defer stream.Close()
	var channelOpenMessage pb.ChannelOpen
	err := ptypes.UnmarshalAny(message.Payload, &channelOpenMessage)
	if err != nil {
		sendErrorMessage(stream, "Invalid message payload")
		return err
	}
	// Make sure the new channel is using a random ID that we don't have in our database
	// We will lock here to make sure no other OpenChannel messages come in using the same
	// channelID before we have a chance to save this one.
	node.lock.Lock()
	defer node.lock.Unlock()
	err = walletdb.View(node.Database, func(tx walletdb.ReadTx) error {
		openBucket := tx.ReadBucket(paymentChannelBucket).NestedReadBucket(openChannelsBucket)
		channel := openBucket.Get([]byte(channelOpenMessage.ChannelID))
		if len(channel) > 0 {
			return errors.New("channelID exists")
		}
		closedBucket := tx.ReadBucket(paymentChannelBucket).NestedReadBucket(closedChannelsBucket)
		channel = closedBucket.Get([]byte(channelOpenMessage.ChannelID))
		if len(channel) > 0 {
			return errors.New("channelID exists")
		}
		return nil
	})
	if err != nil {
		sendErrorMessage(stream, "Invalid channelID")
		return errors.New("new channel open request uses a channelID that already exists in database")
	}
	if channelOpenMessage.DustLimit > uint64(MaxAcceptibleDustLimit) {
		sendErrorMessage(stream, "Unacceptable dust limit")
		return errors.New("new channel open request uses has unacceptable dust limit")
	}
	if channelOpenMessage.Delay > uint32(MaxAcceptibleChannelDelay) {
		sendErrorMessage(stream, "Unacceptable delay")
		return errors.New("new channel open request uses has unacceptable delay")
	}
	if channelOpenMessage.FeePerByte < uint32(MinAcceptibleFeePerByte) {
		sendErrorMessage(stream, "Unacceptable delay")
		return errors.New("new channel open request uses has unacceptable delay")
	}
	remoteChannelPubkey, err := bchec.ParsePubKey(channelOpenMessage.ChannelPubkey, bchec.S256())
	if err != nil {
		sendErrorMessage(stream, "Invalid channel public key")
		return errors.New("new channel open request contained invalid channel public key")
	}
	remoteRevocationPubkey, err := bchec.ParsePubKey(channelOpenMessage.RevocationPubkey, bchec.S256())
	if err != nil {
		sendErrorMessage(stream, "Invalid revocation public key")
		return errors.New("new channel open request contained invalid revocation public key")
	}
	if len(channelOpenMessage.PayoutScript) == 0 {
		sendErrorMessage(stream, "Invalid payout script")
		return errors.New("new channel open request contained invalid payout script")
	}

	channelID, err := chainhash.NewHashFromStr(channelOpenMessage.ChannelID)
	if err != nil {
		sendErrorMessage(stream, "Invalid channel ID")
		return errors.New("new channel open request contained invalid channel ID")
	}
	channel, err := node.initNewIncomingChannel(channelID, channelOpenMessage.AddressID, remoteChannelPubkey,
		remoteRevocationPubkey, bchutil.Amount(channelOpenMessage.DustLimit), bchutil.Amount(channelOpenMessage.FeePerByte),
		channelOpenMessage.Delay, channelOpenMessage.PayoutScript, stream.Conn().RemotePeer())
	if err != nil {
		sendErrorMessage(stream, "Internal node error")
		return fmt.Errorf("error creating new channel:%s", err.Error())
	}

	channelAcceptMessage := pb.ChannelAccept{
		ChannelID:        channel.ChannelID.String(),
		ChannelPubkey:    channel.LocalPrivkey.PubKey().SerializeCompressed(),
		PayoutScript:     channel.LocalPayoutScript,
		RevocationPubkey: channel.LocalRevocationPrivkey.PubKey().SerializeCompressed(),
	}
	m, err := wrapMessage(&channelAcceptMessage, pb.Message_CHANNEL_ACCEPT)
	if err != nil {
		sendErrorMessage(stream, "Internal node error")
		return fmt.Errorf("error creating new channel:%s", err.Error())
	}
	writer := ggio.NewDelimitedWriter(stream)
	err = writer.WriteMsg(m)
	if err != nil {
		return err
	}
	message2, err := readMessageWithTimeout(stream, DefaultNetworkTimeout)
	if err != nil {
		return err
	}
	// We got an error back. Log it and exit.
	if message2.MessageType == pb.Message_ERROR {
		var errorMessage pb.Error
		err := ptypes.UnmarshalAny(message2.Payload, &errorMessage)
		if err == nil {
			log.Error("Received error message from peer %s while trying open channel: %s", stream.Conn().RemotePeer().Pretty(), errorMessage.Message)
			return fmt.Errorf("remote peer %s responded with error message %s", stream.Conn().RemotePeer().Pretty(), errorMessage.Message)
		} else {
			return fmt.Errorf("remote peer %s responsed with error message but we failed to unmarshall the payload", stream.Conn().RemotePeer().Pretty())
		}
	} else if message2.MessageType != pb.Message_INITIAL_COMMIT{ // Malfunctioning remote peer
		sendErrorMessage(stream, "Invalid message type")
		return fmt.Errorf("remote peer %s sent wrong message type", stream.Conn().RemotePeer().Pretty())
	}

	// Unmarshall the InitialCommmit message and make sure all the data we received is correct.
	// If anything is invalid let's send an Error message back and exit.
	var initialCommitMessage pb.InitialCommitment
	err = ptypes.UnmarshalAny(message2.Payload, &initialCommitMessage)
	if err != nil {
		sendErrorMessage(stream, "invalid InitialCommitment message")
		return fmt.Errorf("error unmarshalling initial commitment message from remote peer %s: %s", stream.Conn().RemotePeer().Pretty(), err.Error())
	}
	if initialCommitMessage.ChannelID != channel.ChannelID.String() {
		sendErrorMessage(stream, "channel ID does not match previous message")
		return fmt.Errorf("remote peer %s responded with incorrect channel ID", stream.Conn().RemotePeer().Pretty())
	}
	fundingTxid, err := chainhash.NewHashFromStr(initialCommitMessage.FundingTxid)
	if err != nil {
		sendErrorMessage(stream, "invalid funding txid")
		return fmt.Errorf("remote peer %s responded with invalid funding txid", stream.Conn().RemotePeer().Pretty())
	}
	channel.FundingTxid = *fundingTxid
	channel.RemoteBalance = bchutil.Amount(initialCommitMessage.InitialFundingAmount)
	channel.FundingOutpoint = *wire.NewOutPoint(fundingTxid, initialCommitMessage.FundingIndex)

	_, sig, err := channel.buildCommitmentTransaction(false, node.Params)
	if err != nil {
		return err
	}

	initCommitSigMessage := pb.InitialCommitmentSignature{
		ChannelID: channel.ChannelID.String(),
		Signature: sig,
	}
	m2, err := wrapMessage(&initCommitSigMessage, pb.Message_INITIAL_COMMIT_SIGNATURE)
	if err != nil {
		return err
	}
	err = writer.WriteMsg(m2)
	if err != nil {
		return err
	}

	err = walletdb.Update(node.Database, func(tx walletdb.ReadWriteTx) error {
		bucket := tx.ReadWriteBucket(paymentChannelBucket).NestedReadWriteBucket(openChannelsBucket)
		serializedChannel, err := serializeChannel(*channel)
		if err != nil {
			return err
		}
		return bucket.Put(channel.ChannelID.CloneBytes(), serializedChannel)
	})
	if err != nil {
		return err
	}

	return nil
}

func sendErrorMessage(s inet.Stream, errorString string) {
	errorMessage := pb.Error{
		Message: errorString,
	}
	m, err := wrapMessage(&errorMessage, pb.Message_ERROR)
	if err != nil {
		log.Error("Error sending error message to %s: %s", s.Conn().RemotePeer().Pretty(), err.Error())
		return
	}
	writer := ggio.NewDelimitedWriter(s)
	err = writer.WriteMsg(m)
	if err != nil {
		log.Error("Error sending error message to %s: %s", s.Conn().RemotePeer().Pretty(), err.Error())
	}
}
