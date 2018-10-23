package paymentchannels

import (
	inet "github.com/libp2p/go-libp2p-net"
	"github.com/libp2p/go-libp2p-peer"
	"github.com/libp2p/go-libp2p-protocol"
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
)

// handlingNewStream handles new incoming streams from other peers.
// TODO: build out the stream handler and pull new messages from the stream.
func (node *PaymentChannelNode) handleNewStream(s inet.Stream) {

}

// sendMessage will send a message to another peer.
// TODO: build this out. we should probably make it so we use one stream per channel.
// if the user opens another channel to another node, we will use another stream and
// multiplex it.
func (node *PaymentChannelNode) sendMessage(peerID peer.ID, message []byte) error {
	return nil
}
