package paymentchannels

import (
	"bytes"
	"context"
	"fmt"
	"github.com/gcash/bchd/bchec"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/paymentchannels/pb"
	"github.com/gcash/bchwallet/waddrmgr"
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
	case pb.Message_CHANNEL_UPDATE_PROPOSAL:
		err = node.handleChannelUpdateProposalMessage(&m, s)
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

// handleOpenChannelMessage does the processing for a new channel open message that comes off the
// wire. The sender is the only one putting money in the channel at this point so all we need to
// do is sign his commitment transaction. We don't need to worry about saving our own commitment
// at this point.
func (node *PaymentChannelNode) handleOpenChannelMessage(message *pb.Message, stream inet.Stream) error {
	defer stream.Close()
	var channelOpenMessage pb.ChannelOpen
	err := ptypes.UnmarshalAny(message.Payload, &channelOpenMessage)
	if err != nil {
		sendErrorMessage(stream, "Invalid message payload")
		return err
	}

	// Perform some validation on the incoming message
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

	channel, err := node.initNewIncomingChannel(channelOpenMessage.AddressID, remoteChannelPubkey,
		remoteRevocationPubkey, bchutil.Amount(channelOpenMessage.DustLimit), bchutil.Amount(channelOpenMessage.FeePerByte),
		channelOpenMessage.Delay, channelOpenMessage.PayoutScript, stream.Conn().RemotePeer())
	if err != nil {
		sendErrorMessage(stream, "Internal node error")
		return fmt.Errorf("error creating new channel:%s", err.Error())
	}

	channelAcceptMessage := pb.ChannelAccept{
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
			return fmt.Errorf("remote peer %s responsed with error message but we failed to unmarshal the payload", stream.Conn().RemotePeer().Pretty())
		}
	} else if message2.MessageType != pb.Message_INITIAL_COMMIT { // Malfunctioning remote peer
		sendErrorMessage(stream, "Invalid message type")
		return fmt.Errorf("remote peer %s sent wrong message type", stream.Conn().RemotePeer().Pretty())
	}

	// unmarshal the InitialCommmit message and make sure all the data we received is correct.
	// If anything is invalid let's send an Error message back and exit.
	var initialCommitMessage pb.InitialCommitment
	err = ptypes.UnmarshalAny(message2.Payload, &initialCommitMessage)
	if err != nil {
		sendErrorMessage(stream, "invalid InitialCommitment message")
		return fmt.Errorf("error unmarshaling initial commitment message from remote peer %s: %s", stream.Conn().RemotePeer().Pretty(), err.Error())
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

	err = node.Wallet.ImportAddress(waddrmgr.KeyScopeBIP0044, channel.ChannelAddress, nil, false)
	if err != nil {
		return err
	}
	channel.Status = ChannelStatusOpen

	err = saveChannel(node.Database, channel)
	if err != nil {
		return err
	}
	return nil
}

// handleChannelUpdateProposalMessage does the processing for an incoming request to
// update the channel the channel balance.
func (node *PaymentChannelNode) handleChannelUpdateProposalMessage(message *pb.Message, stream inet.Stream) error {
	defer stream.Close()
	var channelUpdateMessage pb.ChannelUpdateProposal
	err := ptypes.UnmarshalAny(message.Payload, &channelUpdateMessage)
	if err != nil {
		sendErrorMessage(stream, "Invalid message payload")
		return err
	}
	channelID, err := chainhash.NewHashFromStr(channelUpdateMessage.ChannelID)
	if err != nil {
		sendErrorMessage(stream, "Invalid channel ID")
		return err
	}

	node.channelLock.Lock(*channelID)
	defer node.channelLock.Unlock(*channelID)

	channel, err := node.GetChannel(*channelID)
	if err != nil {
		sendErrorMessage(stream, "Invalid channel ID")
		return err
	}
	if channel.RemotePeerID != stream.Conn().RemotePeer() {
		return errors.New("received a channel update message from a peer who is not party to the channel")
	}

	if channelUpdateMessage.Amount > int64(channel.RemoteBalance) || channelUpdateMessage.Amount <= 0 {
		sendErrorMessage(stream, "Invalid amount")
		return err
	}
	channel.RemoteBalance -= bchutil.Amount(channelUpdateMessage.Amount)
	channel.LocalBalance += bchutil.Amount(channelUpdateMessage.Amount)

	newRemoteRevocationPubkey, err := bchec.ParsePubKey(channelUpdateMessage.NewRevocationPubkey, bchec.S256())
	if err != nil {
		sendErrorMessage(stream, "Invalid revocation pubkey")
		return err
	}
	oldRemoteRevocationPubkey := channel.RemoteRevocationPubkey.SerializeCompressed()
	channel.RemoteRevocationPubkey = *newRemoteRevocationPubkey

	// Build our commitment transaction and check that the remote peer sent a valid signature
	newCommitmentTx, localCommitmentSig, err := channel.buildCommitmentTransaction(true, node.Params)
	if err != nil {
		sendErrorMessage(stream, "Invalid commitment signature")
		return err
	}

	var scriptSig []byte
	if channel.Inbound {
		scriptSig, err = buildCommitmentScriptSig(channelUpdateMessage.Signature, localCommitmentSig, channel.RedeemScript)
	} else {
		scriptSig, err = buildCommitmentScriptSig(localCommitmentSig, channelUpdateMessage.Signature, channel.RedeemScript)
	}
	if err != nil {
		sendErrorMessage(stream, "Invalid commitment signature")
		return err
	}
	newCommitmentTx.TxIn[0].SignatureScript = scriptSig
	if !channel.validateCommitmentSignature(newCommitmentTx) {
		sendErrorMessage(stream, "Invalid commitment signature")
		return fmt.Errorf("remote peer %s sent an invalid commitment signature", channel.RemotePeerID.Pretty())
	}
	channel.CommitmentTx = *newCommitmentTx

	// Copy the old revocation key. This will be sent to the remote peer, but we will
	// need to save the new revocation key before signing the remote commitment.
	oldRevocationPrivkey := channel.LocalRevocationPrivkey.Serialize()
	newLocalRevocationPrivkey, err := bchec.NewPrivateKey(bchec.S256())
	if err != nil {
		return err
	}
	channel.LocalRevocationPrivkey = *newLocalRevocationPrivkey

	_, remoteCommitmentSig, err := channel.buildCommitmentTransaction(false, node.Params)
	if err != nil {
		return err
	}

	proposalAcceptMessage := pb.UpdateProposalAccept{
		NewRevocationPubkey: channel.LocalRevocationPrivkey.PubKey().SerializeCompressed(),
		Signature: remoteCommitmentSig,
		RevocationPrivkey: oldRevocationPrivkey,
	}
	m, err := wrapMessage(&proposalAcceptMessage, pb.Message_UPDATE_PROPOSAL_ACCEPT)
	if err != nil {
		return err
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
			return fmt.Errorf("remote peer %s responsed with error message but we failed to unmarshal the payload", stream.Conn().RemotePeer().Pretty())
		}
	} else if message2.MessageType != pb.Message_FINALIZE_UPDATE { // Malfunctioning remote peer
		sendErrorMessage(stream, "Invalid message type")
		return fmt.Errorf("remote peer %s sent wrong message type", stream.Conn().RemotePeer().Pretty())
	}

	// Unmarshal the UpdateProposalAccept message and make sure all the data we received is correct.
	// If anything is invalid let's send an Error message back and exit.
	var finalizeMessage pb.FinalizeUpdate
	err = ptypes.UnmarshalAny(message2.Payload, &finalizeMessage)
	if err != nil {
		sendErrorMessage(stream, "Invalid finalizeMessage message")
		return fmt.Errorf("error unmarshaling finalize message from remote peer %s: %s", stream.Conn().RemotePeer().Pretty(), err.Error())
	}

	// Save the remote peer's private key with the breach address it corresponds to
	remoteRevocationPrivkey, _ := bchec.PrivKeyFromBytes(bchec.S256(), finalizeMessage.RevocationPrivkey)
	if !bytes.Equal(remoteRevocationPrivkey.PubKey().SerializeCompressed(), oldRemoteRevocationPubkey) {
		return fmt.Errorf("remote peer %s sent invalid revocation privkey", stream.Conn().RemotePeer().Pretty())
	}
	breachAddr, _, err := buildBreachRemedyAddress(remoteRevocationPrivkey.PubKey(), channel.LocalPrivkey.PubKey(), &channel.RemotePubkey, channel.Delay, node.Params)
	if err != nil {
		return err
	}
	channel.RemoteRevocationPrivkeys[breachAddr] = *remoteRevocationPrivkey

	err = node.Wallet.ImportAddress(waddrmgr.KeyScopeBIP0044, breachAddr, nil, false)
	if err != nil {
		return err
	}

	channel.TransactionCount++
	err = saveChannel(node.Database, channel)
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
