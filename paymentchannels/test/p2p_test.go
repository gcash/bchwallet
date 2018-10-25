package test

import (
	"crypto/rand"
	"fmt"
	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/paymentchannels"
	"github.com/gcash/bchwallet/walletdb"
	_ "github.com/gcash/bchwallet/walletdb/bdb"
	"github.com/libp2p/go-libp2p-crypto"
	"github.com/libp2p/go-libp2p-peerstore"
	"os"
	"path"
	"testing"
	"time"
)
var alicePath, bobPath string
func TestMain(m *testing.M) {
	alicePath = path.Join(os.TempDir(), "pcAlice")
	bobPath = path.Join(os.TempDir(), "pcBob")

	os.Mkdir(alicePath, os.ModePerm)
	os.Mkdir(bobPath, os.ModePerm)

	exitCode := m.Run()

	os.RemoveAll(alicePath)
	os.RemoveAll(bobPath)

	os.Exit(exitCode)
}

// This is just a basic test. We need to build out the test package more
// and test sending messages around.
func TestNodeConnectivity(t *testing.T) {
	var alicePort, bobPort uint32 = 5001, 5002

	// Create alice's node. No bootstrap nodes for her.
	alicePrivKey, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	aliceDB, err := walletdb.Create("bdb", path.Join(alicePath, "wallet.db"))
	if err != nil {
		t.Fatal(err)
	}
	aliceWallet := NewMockWalletBackend(&chaincfg.RegressionNetParams)
	aliceConfig := paymentchannels.NodeConfig{
		Params:     &chaincfg.RegressionNetParams,
		DataDir:    alicePath,
		PrivateKey: alicePrivKey,
		Port:       alicePort,
		Database:   aliceDB,
		Wallet:     aliceWallet,
	}
	aliceNode, err := paymentchannels.NewPaymentChannelNode(&aliceConfig)
	if err != nil {
		t.Fatal(err)
	}

	// Create bob's node. We'll set alice as a bootstrap peer.
	bobPrivKey, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bobDB, err := walletdb.Create("bdb", path.Join(bobPath, "wallet.db"))
	if err != nil {
		t.Fatal(err)
	}
	bobWallet := NewMockWalletBackend(&chaincfg.RegressionNetParams)
	bobConfig := paymentchannels.NodeConfig{
		Params:     &chaincfg.RegressionNetParams,
		DataDir:    bobPath,
		PrivateKey: bobPrivKey,
		Port:       bobPort,
		Database:   bobDB,
		Wallet:     bobWallet,
		BootstrapPeers: []peerstore.PeerInfo{
			{
				ID:    aliceNode.Host.ID(),
				Addrs: aliceNode.Host.Addrs(),
			},
		},
	}
	bobNode, err := paymentchannels.NewPaymentChannelNode(&bobConfig)
	if err != nil {
		t.Fatal(err)
	}

	// Start up alice and bob
	err = aliceNode.StartOnlineServices()
	if err != nil {
		t.Fatal(err)
	}
	err = bobNode.StartOnlineServices()
	if err != nil {
		t.Fatal(err)
	}

	// Make sure they're connected
	alicePeers := aliceNode.Host.Network().Peers()
	bobPeers := bobNode.Host.Network().Peers()
	if len(alicePeers) == 0 || len(bobPeers) == 0 {
		t.Error("Failed to connect alice to bob")
	}

	bobAddr, err := bobNode.NewAddress()
	if err != nil {
		t.Fatal(err)
	}
	_, err = aliceNode.OpenChannel(bobAddr, bchutil.Amount(10000))
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(time.Second * 3)

	aliceChannels, err := aliceNode.ListChannels()
	if err != nil {
		t.Fatal(err)
	}

	bobChannels, err := bobNode.ListChannels()
	if err != nil {
		t.Fatal(err)
	}

	err = aliceNode.SendPayment(aliceChannels[0].ID, bchutil.Amount(500))
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(time.Second * 3)

	err = aliceNode.SendPayment(aliceChannels[0].ID, bchutil.Amount(1000))
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(time.Second * 3)

	err = bobNode.SendPayment(bobChannels[0].ID, bchutil.Amount(5))
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(time.Second * 3)

	aliceOpenChannel, err := aliceNode.GetChannel(aliceChannels[0].ID)
	if err != nil {
		t.Fatal(err)
	}

	bobOpenChannel, err := bobNode.GetChannel(bobChannels[0].ID)
	if err != nil {
		t.Fatal(err)
	}

	fmt.Println(aliceOpenChannel.String())
	fmt.Println(bobOpenChannel.String())

	t.Log("Boom!!!")
}
