package paymentchannels

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/gcash/bchd/bchec"
	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/paymentchannels/pb"
	"github.com/gcash/bchwallet/waddrmgr"
	"github.com/gcash/bchwallet/wallet/txrules"
	"github.com/gcash/bchwallet/walletdb"
	ggio "github.com/gogo/protobuf/io"
	"github.com/golang/protobuf/ptypes"
	"github.com/ipfs/go-datastore"
	"github.com/ipfs/go-ds-leveldb"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-crypto"
	"github.com/libp2p/go-libp2p-host"
	"github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p-kad-dht/opts"
	"github.com/libp2p/go-libp2p-peerstore"
	"github.com/libp2p/go-libp2p-routing"
	"path"
)

var (
	// ErrorInvalidAddress is returned if the given bchutil.Address is not an AddressPaymentChannel
	ErrorInvalidAddress = errors.New("address must be an AddressPaymentChannel")

	// ErrorUnreachablePeer means we were unable to connect to the request remote peer
	ErrorUnreachablePeer = errors.New("unable to establish connection to remote peer")

	// ErrorChannelNotFound is returned when we can't find the given channel ID in the database
	ErrorChannelNotFound = errors.New("channel not found")

	// ErrorInsufficientFunds is thrown if a channel does not have enough funds to send the given
	// amount.
	ErrorInsufficentFunds = errors.New("insufficient funds")
)

// NodeConfig contains basic configuration information that we'll need to
// start our node.
type NodeConfig struct {
	// Params represents the Bitcoin Cash network that this node will be using.
	Params *chaincfg.Params

	// Port specifies the port use for incoming connections.
	Port uint32

	// BootstrapPeers is a list of peers to use for bootstrapping
	// the DHT and connecting to the network. It's not uncommon for
	// this list to be hardcoded in the app. However, a more robust
	// implementation would persist peers to disk and load them at
	// startup. Additionally, peers could be served via a DNS seed.
	BootstrapPeers []peerstore.PeerInfo

	// PrivateKey is the key to initialize the node with. Typically
	// this will be persisted somewhere and loaded from disk on
	// startup.
	PrivateKey crypto.PrivKey

	// DataDir is the path to a directory to store node data.
	DataDir string

	// Wallet is the bchwallet implementation. We use an interface here
	// just to avoid circular imports.
	Wallet WalletBackend

	// Database is the walletDB where we will store our channel data.
	Database walletdb.DB
}

// PaymentChannelNode represents our node in the overlay network. It is
// capable of making direct connections to other peers in the overlay and
// maintaining a kademlia DHT for the purpose of resolving peerIDs into
// network addresses.
type PaymentChannelNode struct {

	// Params represents the Bitcoin Cash network that this node will be using.
	Params *chaincfg.Params

	// Host is the main libp2p instance which handles all our networking.
	// It will require some configuration to set it up. Once set up we
	// can register new protocol handlers with it.
	Host host.Host

	// Routing is a routing implementation which implements the PeerRouting,
	// ContentRouting, and ValueStore interfaces. In practice this will be
	// a Kademlia DHT.
	Routing routing.IpfsRouting

	// PrivateKey is the identity private key for this node
	PrivateKey crypto.PrivKey

	// Datastore is a datastore implementation that we will use to store routing
	// data.
	Datastore datastore.Datastore

	// Wallet is the bchwallet implementation that we will use to generate addresses
	// and make transactions.
	Wallet WalletBackend

	// Database is the walletdb implementation where we will save our channel data.
	Database walletdb.DB

	// channelLock is a keyed mutex that we will use for locking channels when they
	// are processing update and close messages.
	channelLock Kmutex

	bootstrapPeers []peerstore.PeerInfo
}

// NewPaymentChannelNode is a constructor for our Node object
func NewPaymentChannelNode(config *NodeConfig) (*PaymentChannelNode, error) {
	opts := []libp2p.Option{
		// Listen on all interface on both IPv4 and IPv6.
		// If we're going to enable other transports such as Tor or QUIC we would do it here.

		// TODO: users who start in Tor mode will have their privacy blown if they use this
		// before getting around to implementing Tor. For now we should probably check if
		// the wallet was started in Tor mode and panic if payment channels are enabled.
		libp2p.ListenAddrStrings(fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", config.Port)),
		libp2p.ListenAddrStrings(fmt.Sprintf("/ip6/::/tcp/%d", config.Port)),
		libp2p.Identity(config.PrivateKey),
	}

	// This function will initialize a new libp2p host with our options plus a bunch of default options
	// The default options includes default transports, muxers, security, and peer store.
	peerHost, err := libp2p.New(context.Background(), opts...)
	if err != nil {
		return nil, err
	}

	// Create a leveldb datastore
	dstore, err := leveldb.NewDatastore(path.Join(config.DataDir, "libp2p"), nil)
	if err != nil {
		return nil, err
	}

	// Create the DHT instance. It needs the host and a datastore instance.
	routing, err := dht.New(
		context.Background(), peerHost,
		dhtopts.Datastore(dstore),
		dhtopts.Protocols(ProtocolDHT),
	)

	node := &PaymentChannelNode{
		Params:         config.Params,
		Host:           peerHost,
		Routing:        routing,
		PrivateKey:     config.PrivateKey,
		Datastore:      dstore,
		Wallet:         config.Wallet,
		Database:       config.Database,
		bootstrapPeers: config.BootstrapPeers,
		channelLock:    NewKmutex(),
	}
	if err = initDatabase(node.Database); err != nil {
		return nil, err
	}
	// Register the paymentchannel protocol with the host
	peerHost.SetStreamHandler(ProtocolPaymnetChannel, node.handleNewStream)
	return node, nil
}

// StartOnlineServices will bootstrap the peer host using the provided bootstrap peers. Once the host
// has been bootstrapped it will proceed to bootstrap the DHT.
func (n *PaymentChannelNode) StartOnlineServices() error {
	return Bootstrap(n.Routing.(*dht.IpfsDHT), n.Host, bootstrapConfigWithPeers(n.bootstrapPeers))
}

// Shutdown will cancel the context shared by the various components which will shut them all down
// disconnecting all peers in the process.
func (n *PaymentChannelNode) Shutdown() {
	n.Host.Close()
}

// NewAddress returns a new AddressPaymentChannel with a random address ID.
func (n *PaymentChannelNode) NewAddress() (*bchutil.AddressPaymentChannel, error) {
	addrID := make([]byte, 16)
	rand.Read(addrID)
	pubkey, err := n.Host.ID().ExtractPublicKey()
	if err != nil {
		return nil, err
	}
	rawPubKey, err := pubkey.Raw()
	if err != nil {
		return nil, err
	}
	return bchutil.NewAddressPaymentChannel(rawPubKey, addrID, n.Params)
}

// OpenChannel steps through the channel opening protocol. At the end of the function
// we will either have an open, funded channel to the other node, or it will have failed.
//    +-------+                                       +-------+
//    |       |--(1)---------   OpenChannel   ------->|       |
//    |       |<-(2)--------   ChannelAccept  --------|       |
//    |   A   |                                       |   B   |
//    |       |--(3)------  InitialCommitment  ------>|       |
//    |       |<-(4)--- InitialCommitmentSignature ---|       |
//    +-------+                                       +-------+
//
//    - where node A is 'funder' and node B is 'fundee'
func (n *PaymentChannelNode) OpenChannel(addr bchutil.Address, amount bchutil.Amount) (*chainhash.Hash, error) {
	// We're going to create a dummy P2SH script as a place holder because we don't
	// yet know the remote peer's public key to build the correct P2SH output script.
	dummyScript := make([]byte, 24)

	// Creating a dummy tx allows us to both check that we have enough funds in the wallet
	// to open the channel for the given amount and get the outpoints from the dummy transaction
	// so we can lock them for later use.
	txout := wire.NewTxOut(int64(amount), dummyScript)
	dummyTx, err := n.Wallet.CreateSimpleTx(0, []*wire.TxOut{txout}, 0, txrules.DefaultRelayFeePerKb)
	if err != nil {
		return nil, err
	}
	for _, in := range dummyTx.Tx.TxIn {
		n.Wallet.LockOutpoint(in.PreviousOutPoint)
	}

	// Unlock the outputs if we exit early due to an error
	defer func() {
		for _, in := range dummyTx.Tx.TxIn {
			n.Wallet.UnlockOutpoint(in.PreviousOutPoint)
		}
	}()

	// Assert that the bchutil.Address that was passed in is a payment channel address
	paymentChannelAddr, ok := addr.(*bchutil.AddressPaymentChannel)
	if !ok {
		return nil, ErrorInvalidAddress
	}

	// Build a new outgoing channel with default data
	channel, err := n.initNewOutgoingChannel(paymentChannelAddr, amount)
	if err != nil {
		return nil, err
	}

	// We're going to open a new stream to this peer. The way we are going
	// to handle networking is we are going to use one stream for the entire
	// channel initiation.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := n.openStream(ctx, channel.RemotePeerID)
	if err != nil {
		return nil, ErrorUnreachablePeer
	}
	// Close the stream when we're done here. New channel actions will get a
	// new stream.
	defer stream.Close()

	// Build ChannelOpen protobuf message
	channelOpenMessage := pb.ChannelOpen{
		AddressID:        channel.AddressID,
		ChannelPubkey:    channel.LocalPrivkey.PubKey().SerializeCompressed(),
		Delay:            uint32(channel.Delay),
		FeePerByte:       uint32(channel.FeePerByte),
		DustLimit:        uint64(channel.DustLimit),
		PayoutScript:     channel.LocalPayoutScript,
		RevocationPubkey: channel.LocalRevocationPrivkey.PubKey().SerializeCompressed(),
	}
	m, err := wrapMessage(&channelOpenMessage, pb.Message_CHANNEL_OPEN)
	if err != nil {
		return nil, err
	}

	// Messages are varint delimited. Let's send our message to the other
	// peer. If this fails it's no big deal as we haven't saved anything yet.
	writer := ggio.NewDelimitedWriter(stream)
	err = writer.WriteMsg(m)
	if err != nil {
		return nil, err
	}

	// The remote peer is supposed to send us a message back. The response
	// can either be a ChannelAccept message or an Error.
	message, err := readMessageWithTimeout(stream, DefaultNetworkTimeout)
	if err != nil {
		return nil, err
	}

	// We got an error back. Log it and exit.
	if message.MessageType == pb.Message_ERROR {
		var errorMessage pb.Error
		err := ptypes.UnmarshalAny(message.Payload, &errorMessage)
		if err == nil {
			log.Error("Received error message from peer %s while trying open channel: %s", stream.Conn().RemotePeer().Pretty(), errorMessage.Message)
			return nil, fmt.Errorf("remote peer %s responded with error message %s", stream.Conn().RemotePeer().Pretty(), errorMessage.Message)
		} else {
			return nil, fmt.Errorf("remote peer %s responsed with error message but we failed to unmarshal the payload", stream.Conn().RemotePeer().Pretty())
		}
	} else if message.MessageType != pb.Message_CHANNEL_ACCEPT { // Malfunctioning remote peer
		sendErrorMessage(stream, "Invalid message type")
		return nil, fmt.Errorf("remote peer %s sent wrong message type", stream.Conn().RemotePeer().Pretty())
	}

	// unmarshal the ChannelAccept message and make sure all the data we received is correct.
	// If anything is invalid let's send an Error message back and exit.
	var channelAcceptMessage pb.ChannelAccept
	err = ptypes.UnmarshalAny(message.Payload, &channelAcceptMessage)
	if err != nil {
		sendErrorMessage(stream, "invalid ChannelAccept message")
		return nil, fmt.Errorf("error unmarshaling accept message from remote peer %s: %s", stream.Conn().RemotePeer().Pretty(), err.Error())
	}
	remoteChannelPubkey, err := bchec.ParsePubKey(channelAcceptMessage.ChannelPubkey, bchec.S256())
	if err != nil {
		sendErrorMessage(stream, "invalid channel public key")
		return nil, fmt.Errorf("remote peer %s sent invalid channel public key: %s", stream.Conn().RemotePeer().Pretty(), err.Error())
	}
	remoteRevocationPubkey, err := bchec.ParsePubKey(channelAcceptMessage.RevocationPubkey, bchec.S256())
	if err != nil {
		sendErrorMessage(stream, "invalid revocation public key")
		return nil, fmt.Errorf("remote peer %s sent invalid revocation public key: %s", stream.Conn().RemotePeer().Pretty(), err.Error())
	}
	if len(channelAcceptMessage.PayoutScript) == 0 {
		sendErrorMessage(stream, "invalid payout script")
		return nil, fmt.Errorf("remote peer %s sent invalid payout script", stream.Conn().RemotePeer().Pretty())
	}
	channel.RemotePubkey = *remoteChannelPubkey
	channel.RemoteRevocationPubkey = *remoteRevocationPubkey
	channel.RemotePayoutScript = channelAcceptMessage.PayoutScript

	// The channelID is the big endian sha256 hash of cat(openingPeerPublicKey, otherPeerPublicKey)
	// chainhash.NewHash() will reverse the byte order if we use that function which is why we
	// encode to hex first and use NewHashFromStr() which reserves the byte order.
	cidBytes := sha256.Sum256(append(channel.LocalPrivkey.PubKey().SerializeCompressed(), channel.RemotePubkey.SerializeCompressed()...))
	channelID, err := chainhash.NewHashFromStr(hex.EncodeToString(cidBytes[:]))
	if err != nil {
		return nil, err
	}
	channel.ID = *channelID

	// Build the 2 of 2 multisig P2SH address where we are going to send the funds to open the channel
	// The channel opener's public key always goes first.
	channelAddr, redeemScript, err := buildP2SHAddress(channel.LocalPrivkey.PubKey(), &channel.RemotePubkey, n.Params)
	if err != nil {
		return nil, err
	}
	channel.ChannelAddress = channelAddr
	channel.RedeemScript = redeemScript

	outputScript, err := txscript.PayToAddrScript(channelAddr)
	if err != nil {
		return nil, err
	}
	// We can remove the outpoint lock long enough to re-build the transaction with the correct output
	for _, in := range dummyTx.Tx.TxIn {
		n.Wallet.UnlockOutpoint(in.PreviousOutPoint)
	}
	fundingTxOut := wire.NewTxOut(int64(amount), outputScript)
	fundingTx, err := n.Wallet.CreateSimpleTx(0, []*wire.TxOut{fundingTxOut}, 0, txrules.DefaultRelayFeePerKb)
	if err != nil {
		return nil, err
	}
	fundingTxid := fundingTx.Tx.TxHash()
	// Now lock the outpoints again so that no other transactions use them while we're
	// finishing up channel creation.
	for _, in := range fundingTx.Tx.TxIn {
		n.Wallet.LockOutpoint(in.PreviousOutPoint)
	}
	fundingIndex := 0
	for i, out := range fundingTx.Tx.TxOut {
		if bytes.Equal(out.PkScript, outputScript) {
			fundingIndex = i
			break
		}
	}

	// Unlock outpoints if we exit early due to an error
	defer func() {
		for _, in := range fundingTx.Tx.TxIn {
			n.Wallet.UnlockOutpoint(in.PreviousOutPoint)
		}
	}()

	channel.FundingTxid = fundingTxid
	channel.FundingOutpoint = *wire.NewOutPoint(&fundingTxid, uint32(fundingIndex))

	initialCommitment := pb.InitialCommitment{
		FundingTxid:          fundingTxid.String(),
		InitialFundingAmount: uint64(amount),
		FundingIndex:         uint32(fundingIndex),
	}
	m2, err := wrapMessage(&initialCommitment, pb.Message_INITIAL_COMMIT)
	if err != nil {
		return nil, err
	}
	err = writer.WriteMsg(m2)
	if err != nil {
		return nil, err
	}
	message2, err := readMessageWithTimeout(stream, DefaultNetworkTimeout)
	if err != nil {
		return nil, err
	}
	// We got an error back. Log it and exit.
	if message2.MessageType == pb.Message_ERROR {
		var errorMessage pb.Error
		err := ptypes.UnmarshalAny(message2.Payload, &errorMessage)
		if err == nil {
			log.Error("Received error message from peer %s while trying open channel: %s", stream.Conn().RemotePeer().Pretty(), errorMessage.Message)
			return nil, fmt.Errorf("remote peer %s responded with error message %s", stream.Conn().RemotePeer().Pretty(), errorMessage.Message)
		} else {
			return nil, fmt.Errorf("remote peer %s responsed with error message but we failed to unmarshal the payload", stream.Conn().RemotePeer().Pretty())
		}
	} else if message2.MessageType != pb.Message_INITIAL_COMMIT_SIGNATURE { // Malfunctioning remote peer
		return nil, fmt.Errorf("remote peer %s sent wrong message type", stream.Conn().RemotePeer().Pretty())
	}

	// Unmarshal the ChannelAccept message and make sure all the data we received is correct.
	// If anything is invalid let's send an Error message back and exit.
	var initCommitSigMessage pb.InitialCommitmentSignature
	err = ptypes.UnmarshalAny(message2.Payload, &initCommitSigMessage)
	if err != nil {
		return nil, fmt.Errorf("error unmarshaling init commit signature message from remote peer %s: %s", stream.Conn().RemotePeer().Pretty(), err.Error())
	}

	commitmentTx, sig, err := channel.buildCommitmentTransaction(true, n.Params)
	if err != nil {
		return nil, err
	}
	scriptSig, err := buildCommitmentScriptSig(sig, initCommitSigMessage.Signature, channel.RedeemScript)
	if err != nil {
		return nil, err
	}
	commitmentTx.TxIn[0].SignatureScript = scriptSig

	if !channel.validateCommitmentSignature(commitmentTx) {
		log.Errorf("Remote peer %s send invalid signature on initial commitment transaction", stream.Conn().RemotePeer().Pretty())
		return nil, fmt.Errorf("invalid signature on initial commit from %s", stream.Conn().RemotePeer().Pretty())
	}
	channel.CommitmentTx = *commitmentTx
	channel.Status = ChannelStatusOpen

	err = n.Wallet.ImportAddress(waddrmgr.KeyScopeBIP0044, channelAddr, nil, false)
	if err != nil {
		return nil, err
	}

	err = saveChannel(n.Database, channel)
	if err != nil {
		return nil, err
	}
	return &fundingTxid, n.Wallet.PublishTransaction(fundingTx.Tx)
}

// Send payment will send a payment to the remote peer via an open channel
func (n *PaymentChannelNode) SendPayment(channelID chainhash.Hash, amount bchutil.Amount) error {
	n.channelLock.Lock(channelID)
	defer n.channelLock.Unlock(channelID)

	channel, err := n.GetChannel(channelID)
	if err != nil {
		return err
	}
	if channel.Status != ChannelStatusOpen {
		return errors.New("cannot send payment when channel is not open")
	}
	if channel.LocalBalance < amount {
		return ErrorInsufficentFunds
	}
	if amount <= 0 {
		return errors.New("amount must be positive")
	}
	channel.LocalBalance -= amount
	channel.RemoteBalance += amount

	// Create a new revocation private key
	newRevocationPrivkey, err := bchec.NewPrivateKey(bchec.S256())
	if err != nil {
		return err
	}
	oldRevocataionPrivkey := channel.LocalRevocationPrivkey.Serialize()
	channel.LocalRevocationPrivkey = *newRevocationPrivkey

	// Build and sign new commitment transaction for remote peer
	_, remoteCommitmentSig, err := channel.buildCommitmentTransaction(false, n.Params)
	if err != nil {
		return err
	}

	// Build the channel update message and send it to the remote peer
	channelUpdateMessage := pb.ChannelUpdateProposal{
		Amount: int64(amount),
		ChannelID: channel.ID.String(),
		NewRevocationPubkey: newRevocationPrivkey.PubKey().SerializeCompressed(),
		Signature: remoteCommitmentSig,
	}
	m, err := wrapMessage(&channelUpdateMessage, pb.Message_CHANNEL_UPDATE_PROPOSAL)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := n.openStream(ctx, channel.RemotePeerID)
	if err != nil {
		return err
	}
	writer := ggio.NewDelimitedWriter(stream)
	err = writer.WriteMsg(m)
	if err != nil {
		return err
	}

	message, err := readMessageWithTimeout(stream, DefaultNetworkTimeout)
	if err != nil {
		return err
	}

	// We got an error back. Log it and exit.
	if message.MessageType == pb.Message_ERROR {
		var errorMessage pb.Error
		err := ptypes.UnmarshalAny(message.Payload, &errorMessage)
		if err == nil {
			log.Error("Received error message from peer %s while trying open channel: %s", stream.Conn().RemotePeer().Pretty(), errorMessage.Message)
			return fmt.Errorf("remote peer %s responded with error message %s", stream.Conn().RemotePeer().Pretty(), errorMessage.Message)
		} else {
			return fmt.Errorf("remote peer %s responsed with error message but we failed to unmarshal the payload", stream.Conn().RemotePeer().Pretty())
		}
	} else if message.MessageType != pb.Message_UPDATE_PROPOSAL_ACCEPT { // Malfunctioning remote peer
		sendErrorMessage(stream, "Invalid message type")
		return fmt.Errorf("remote peer %s sent wrong message type", stream.Conn().RemotePeer().Pretty())
	}

	// Unmarshal the UpdateProposalAccept message and make sure all the data we received is correct.
	// If anything is invalid let's send an Error message back and exit.
	var updateAcceptMessage pb.UpdateProposalAccept
	err = ptypes.UnmarshalAny(message.Payload, &updateAcceptMessage)
	if err != nil {
		sendErrorMessage(stream, "Invalid UpdateProposalAccept message")
		return fmt.Errorf("error unmarshaling accept message from remote peer %s: %s", stream.Conn().RemotePeer().Pretty(), err.Error())
	}

	// Save the remote peer's private key with the breach address it corresponds to
	remoteRevocationPrivkey, _ := bchec.PrivKeyFromBytes(bchec.S256(), updateAcceptMessage.RevocationPrivkey)
	if !bytes.Equal(remoteRevocationPrivkey.PubKey().SerializeCompressed(), channel.RemoteRevocationPubkey.SerializeCompressed()){
		sendErrorMessage(stream, "Invalid revocation privkey")
		return fmt.Errorf("remote peer %s sent invalid revocation privkey", stream.Conn().RemotePeer().Pretty())
	}
	// For the initial commitment the remote peer doesn't have a commitment transaction so there's no need
	// to save the private key
	var breachAddr bchutil.Address
	if !channel.Inbound && channel.TransactionCount > 0 {
		breachAddr, _, err = buildBreachRemedyAddress(remoteRevocationPrivkey.PubKey(), channel.LocalPrivkey.PubKey(), &channel.RemotePubkey, channel.Delay, n.Params)
		if err != nil {
			return err
		}
		channel.RemoteRevocationPrivkeys[breachAddr] = *remoteRevocationPrivkey
	}

	newRemoteRevocationPubkey, err := bchec.ParsePubKey(updateAcceptMessage.NewRevocationPubkey, bchec.S256())
	if err != nil {
		sendErrorMessage(stream, "Invalid revocation pubkey")
		return fmt.Errorf("remote peer %s sent invalid revocation pubkey", stream.Conn().RemotePeer().Pretty())
	}
	channel.RemoteRevocationPubkey = *newRemoteRevocationPubkey

	newLocalCommitmentTx, localCommitmentSig, err := channel.buildCommitmentTransaction(true, n.Params)
	if err != nil {
		return err
	}
	var scriptSig []byte
	if !channel.Inbound {
		scriptSig, err = buildCommitmentScriptSig(localCommitmentSig, updateAcceptMessage.Signature, channel.RedeemScript)
	} else {
		scriptSig, err = buildCommitmentScriptSig(updateAcceptMessage.Signature, localCommitmentSig, channel.RedeemScript)
	}
	if err != nil {
		return err
	}
	newLocalCommitmentTx.TxIn[0].SignatureScript = scriptSig

	if !channel.validateCommitmentSignature(newLocalCommitmentTx) {
		sendErrorMessage(stream, "Invalid commitment signature")
		return fmt.Errorf("remote peer %s sent an invalid commitment signature", channel.RemotePeerID.Pretty())
	}
	channel.CommitmentTx = *newLocalCommitmentTx

	finalizeMessage := pb.FinalizeUpdate{
		RevocationPrivkey: oldRevocataionPrivkey,
	}
	m2, err := wrapMessage(&finalizeMessage, pb.Message_FINALIZE_UPDATE)
	if err != nil {
		return err
	}
	err = writer.WriteMsg(m2)
	if err != nil {
		return err
	}

	if breachAddr != nil {
		err = n.Wallet.ImportAddress(waddrmgr.KeyScopeBIP0044, breachAddr, nil, false)
		if err != nil {
			return err
		}
	}

	channel.TransactionCount++
	err = saveChannel(n.Database, channel)
	if err != nil {
		return err
	}
	return nil
}

// stub
func (n *PaymentChannelNode) CloseChannel() {}

// ListChannels returns a slice of both open and closed channels
func (n *PaymentChannelNode) ListChannels() ([]Channel, error) {
	var channels []Channel
	err := walletdb.View(n.Database, func(tx walletdb.ReadTx) error {
		openBucket := tx.ReadBucket(paymentChannelBucket).NestedReadBucket(openChannelsBucket)
		err := openBucket.ForEach(func(k, v []byte) error {
			ch, err := deserializeChannel(v, n.Params)
			if err != nil {
				return err
			}
			channels = append(channels, *ch)
			return nil
		})
		if err != nil {
			return err
		}
		closedBucket := tx.ReadBucket(paymentChannelBucket).NestedReadBucket(closedChannelsBucket)
		err = closedBucket.ForEach(func(k, v []byte) error {
			ch, err := deserializeChannel(v, n.Params)
			if err != nil {
				return err
			}
			channels = append(channels, *ch)
			return nil
		})
		if err != nil {
			return err
		}
		return nil
	})
	return channels, err
}

// stub
func (n *PaymentChannelNode) ListTransactions() {}

// GetChannel returns the channel for a given ID
func (n *PaymentChannelNode) GetChannel(channelID chainhash.Hash) (*Channel, error) {
	var channel *Channel
	var err error
	err = walletdb.View(n.Database, func(tx walletdb.ReadTx) error {
		openBucket := tx.ReadBucket(paymentChannelBucket).NestedReadBucket(openChannelsBucket)
		channelBytes := openBucket.Get(channelID.CloneBytes())
		channel, err = deserializeChannel(channelBytes, n.Params)
		if err == nil {
			return nil
		}
		closedBucket := tx.ReadBucket(paymentChannelBucket).NestedReadBucket(closedChannelsBucket)
		channelBytes = closedBucket.Get(channelID.CloneBytes())
		channel, err = deserializeChannel(channelBytes, n.Params)
		return err
	})
	if err != nil {
		return nil, ErrorChannelNotFound
	}
	return channel, nil
}
