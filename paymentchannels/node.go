package paymentchannels

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/gcash/bchd/bchec"
	"github.com/gcash/bchd/chaincfg"
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
	"sync"
)

var (
	// ErrorInvalidAddress is returned if the given bchutil.Address is not an AddressPaymentChannel
	ErrorInvalidAddress = errors.New("address must be an AddressPaymentChannel")

	// ErrorUnreachablePeer means we were unable to connect to the request remote peer
	ErrorUnreachablePeer = errors.New("unable to establish connection to remote peer")
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

	bootstrapPeers []peerstore.PeerInfo
	lock           sync.RWMutex
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
		lock:           sync.RWMutex{},
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
func (n *PaymentChannelNode) OpenChannel(addr bchutil.Address, amount bchutil.Amount) error {
	// We're going to create a dummy P2SH script as a place holder because we don't
	// yet know the remote peer's public key to build the correct P2SH output script.
	dummyScript := make([]byte, 24)

	// Creating a dummy tx allows us to both check that we have enough funds in the wallet
	// to open the channel for the given amount and get the outpoints from the dummy transaction
	// so we can lock them for later use.
	txout := wire.NewTxOut(int64(amount), dummyScript)
	dummyTx, err := n.Wallet.CreateSimpleTx(0, []*wire.TxOut{txout}, 0, txrules.DefaultRelayFeePerKb)
	if err != nil {
		return err
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
		return ErrorInvalidAddress
	}

	// Build a new outgoing channel with default data
	channel, err := n.initNewOutgoingChannel(paymentChannelAddr, amount)
	if err != nil {
		return err
	}

	// We're going to open a new stream to this peer. The way we are going
	// to handle networking is we are going to use one stream for the entire
	// channel initiation.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := n.openStream(ctx, channel.RemotePeerID)
	if err != nil {
		fmt.Println(err)
		return ErrorUnreachablePeer
	}
	// Close the stream when we're done here. New channel actions will get a
	// new stream.
	defer stream.Close()

	// Build ChannelOpen protobuf message
	channelOpenMessage := pb.ChannelOpen{
		ChannelID:            channel.ChannelID.String(),
		AddressID:            channel.AddressID,
		ChannelPubkey:        channel.LocalPrivkey.PubKey().SerializeCompressed(),
		Delay:                uint32(channel.Delay),
		FeePerByte:           uint32(channel.FeePerByte),
		DustLimit:            uint64(channel.DustLimit),
		PayoutScript:         channel.LocalPayoutScript,
		RevocationPubkey:     channel.LocalRevocationPrivkey.PubKey().SerializeCompressed(),
	}
	m, err := wrapMessage(&channelOpenMessage, pb.Message_CHANNEL_OPEN)
	if err != nil {
		return err
	}

	// Messages are varint delimited. Let's send our message to the other
	// peer. If this fails it's no big deal as we haven't saved anything yet.
	writer := ggio.NewDelimitedWriter(stream)
	err = writer.WriteMsg(m)
	if err != nil {
		return err
	}

	// The remote peer is supposed to send us a message back. The response
	// can either be a ChannelAccept message or an Error.
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
			return fmt.Errorf("remote peer %s responsed with error message but we failed to unmarshall the payload", stream.Conn().RemotePeer().Pretty())
		}
	} else if message.MessageType != pb.Message_CHANNEL_ACCEPT { // Malfunctioning remote peer
		sendErrorMessage(stream, "Invalid message type")
		return fmt.Errorf("remote peer %s sent wrong message type", stream.Conn().RemotePeer().Pretty())
	}

	// Unmarshall the ChannelAccept message and make sure all the data we received is correct.
	// If anything is invalid let's send an Error message back and exit.
	var channelAcceptMessage pb.ChannelAccept
	err = ptypes.UnmarshalAny(message.Payload, &channelAcceptMessage)
	if err != nil {
		sendErrorMessage(stream, "invalid ChannelAccept message")
		return fmt.Errorf("error unmarshalling accept message from remote peer %s: %s", stream.Conn().RemotePeer().Pretty(), err.Error())
	}
	if channelAcceptMessage.ChannelID != channel.ChannelID.String() {
		sendErrorMessage(stream, "channel ID does not match previous message")
		return fmt.Errorf("remote peer %s responded with incorrect channel ID", stream.Conn().RemotePeer().Pretty())
	}
	remoteChannelPubkey, err := bchec.ParsePubKey(channelAcceptMessage.ChannelPubkey, bchec.S256())
	if err != nil {
		sendErrorMessage(stream, "invalid channel public key")
		return fmt.Errorf("remote peer %s sent invalid channel public key: %s", stream.Conn().RemotePeer().Pretty(), err.Error())
	}
	remoteRevocationPubkey, err := bchec.ParsePubKey(channelAcceptMessage.RevocationPubkey, bchec.S256())
	if err != nil {
		sendErrorMessage(stream, "invalid revocation public key")
		return fmt.Errorf("remote peer %s sent invalid revocation public key: %s", stream.Conn().RemotePeer().Pretty(), err.Error())
	}
	if len(channelAcceptMessage.PayoutScript) == 0 {
		sendErrorMessage(stream, "invalid payout script")
		return fmt.Errorf("remote peer %s sent invalid payout script", stream.Conn().RemotePeer().Pretty())
	}
	channel.RemotePubkey = *remoteChannelPubkey
	channel.RemoteRevocationPubkey = *remoteRevocationPubkey
	channel.RemotePayoutScript = channelAcceptMessage.PayoutScript

	// Build the 2 of 2 multisig P2SH address where we are going to send the funds to open the channel
	// The channel opener's public key always goes first.
	channelAddr, redeemScript, err := buildP2SHAddress(channel.LocalPrivkey.PubKey(), &channel.RemotePubkey, n.Params)
	if err != nil {
		return err
	}
	channel.ChannelAddress = channelAddr.String()
	channel.RedeemScript = redeemScript

	outputScript, err := txscript.PayToAddrScript(channelAddr)
	if err != nil {
		return err
	}
	// We can remove the outpoint lock long enough to re-build the transaction with the correct output
	for _, in := range dummyTx.Tx.TxIn {
		n.Wallet.UnlockOutpoint(in.PreviousOutPoint)
	}
	fundingTxOut := wire.NewTxOut(int64(amount), outputScript)
	fundingTx, err := n.Wallet.CreateSimpleTx(0, []*wire.TxOut{fundingTxOut}, 0, txrules.DefaultRelayFeePerKb)
	if err != nil {
		return err
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

	channel.FundingOutpoint = *wire.NewOutPoint(&fundingTxid, uint32(fundingIndex))

	initialCommitment := pb.InitialCommitment{
		ChannelID: channel.ChannelID.String(),
		FundingTxid: fundingTxid.String(),
		InitialFundingAmount: uint64(amount),
		FundingIndex: uint32(fundingIndex),
	}
	m2, err := wrapMessage(&initialCommitment, pb.Message_INITIAL_COMMIT)
	if err != nil {
		return err
	}
	err = writer.WriteMsg(m2)
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
	} else if message2.MessageType != pb.Message_INITIAL_COMMIT_SIGNATURE { // Malfunctioning remote peer
		return fmt.Errorf("remote peer %s sent wrong message type", stream.Conn().RemotePeer().Pretty())
	}

	// Unmarshall the ChannelAccept message and make sure all the data we received is correct.
	// If anything is invalid let's send an Error message back and exit.
	var initCommitSigMessage pb.InitialCommitmentSignature
	err = ptypes.UnmarshalAny(message2.Payload, &initCommitSigMessage)
	if err != nil {
		return fmt.Errorf("error unmarshalling init commit signature message from remote peer %s: %s", stream.Conn().RemotePeer().Pretty(), err.Error())
	}

	commitmentTx, sig, err := channel.buildCommitmentTransaction(true, n.Params)
	if err != nil {
		return err
	}
	scriptSig, err := buildCommitmentScriptSig(sig, initCommitSigMessage.Signature, channel.RedeemScript)
	if err != nil {
		return err
	}
	commitmentTx.TxIn[0].SignatureScript = scriptSig

	if !channel.validateCommitmentSignature(commitmentTx, n.Params) {
		log.Errorf("Remote peer %s send invalid signature on initial commitment transaction", stream.Conn().RemotePeer().Pretty())
		return fmt.Errorf("invalid signature on initial commit from %s", stream.Conn().RemotePeer().Pretty())
	}
	channel.CommitmentTx = *commitmentTx
	channel.State = ChannelStateOpen

	err = n.Wallet.ImportAddress(waddrmgr.KeyScopeBIP0044, channelAddr, nil, false)
	if err != nil {
		return err
	}

	err = walletdb.Update(n.Database, func(tx walletdb.ReadWriteTx) error {
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
	return n.Wallet.PublishTransaction(fundingTx.Tx)
}

// stub
func (n *PaymentChannelNode) SendPayment() {}

// stub
func (n *PaymentChannelNode) CloseChannel() {}

// stub
func (n *PaymentChannelNode) ListChannels() {}

// stub
func (n *PaymentChannelNode) ListTransactions() {}
