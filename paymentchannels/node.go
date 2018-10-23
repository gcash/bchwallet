package paymentchannels

import (
	"context"
	"fmt"
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

// NodeConfig contains basic configuration information that we'll need to
// start our node.
type NodeConfig struct {
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
}

// PaymentChannelNode represents our node in the overlay network. It is
// capable of making direct connections to other peers in the overlay and
// maintaining a kademlia DHT for the purpose of resolving peerIDs into
// network addresses.
type PaymentChannelNode struct {

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
		Host:           peerHost,
		Routing:        routing,
		PrivateKey:     config.PrivateKey,
		Datastore:      dstore,
		Wallet:         config.Wallet,
		bootstrapPeers: config.BootstrapPeers,
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

// stub
func (n *PaymentChannelNode) NewAddress() {}

// stub
func (n *PaymentChannelNode) OpenChannel() {}

// stub
func (n *PaymentChannelNode) SendPayment() {}

// stub
func (n *PaymentChannelNode) CloseChannel() {}

// stub
func (n *PaymentChannelNode) ListChannels() {}

// stub
func (n *PaymentChannelNode) ListTransactions() {}
