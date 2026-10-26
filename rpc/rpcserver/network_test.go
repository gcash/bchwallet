package rpcserver

import (
	"context"
	"testing"
	"time"

	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchutil/hdkeychain"
	"github.com/gcash/bchwallet/chain"
	pb "github.com/gcash/bchwallet/rpc/walletrpc"
	"github.com/gcash/bchwallet/wallet"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeChain is a chain backend with the best height and sync state a test
// sets, like a bchd RPC backend, which does not report its peers.
type fakeChain struct {
	chain.Interface
	height  int32
	current bool
}

func (c *fakeChain) GetBestBlock() (*chainhash.Hash, int32, error) {
	return &chainhash.Hash{}, c.height, nil
}

func (c *fakeChain) IsCurrent() bool { return c.current }

func (c *fakeChain) Notifications() <-chan interface{} { return nil }

func (c *fakeChain) Stop() {}

func (c *fakeChain) WaitForShutdown() {}

// fakeSPVChain also reports its peers, as the SPV backend does.
type fakeSPVChain struct {
	fakeChain
	peers      int
	peerHeight int32
}

func (c *fakeSPVChain) PeerInfo() (int, int32) { return c.peers, c.peerHeight }

// newNetworkTestServer returns a wallet server for a new mainnet wallet,
// synced with client unless it is nil.
func newNetworkTestServer(t *testing.T, client chain.Interface) *walletServer {
	t.Helper()

	seed, err := hdkeychain.GenerateSeed(hdkeychain.MinSeedBytes)
	if err != nil {
		t.Fatal(err)
	}
	loader := wallet.NewLoader(&chaincfg.MainNetParams, t.TempDir(), true, 250)
	w, err := loader.CreateNewWallet([]byte("public"), []byte("private"), seed,
		time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := loader.UnloadWallet(); err != nil {
			t.Error(err)
		}
	})
	if client != nil {
		w.SynchronizeRPC(client)
	}
	return &walletServer{wallet: w}
}

func TestNetwork(t *testing.T) {
	ctx := context.Background()

	s := newNetworkTestServer(t, nil)
	if _, err := s.Network(ctx, &pb.NetworkRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("without a chain backend: got %v, want FailedPrecondition", err)
	}

	s = newNetworkTestServer(t, &fakeChain{height: 974403})
	resp, err := s.Network(ctx, &pb.NetworkRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ActiveNetwork != uint32(chaincfg.MainNetParams.Net) ||
		resp.BestHeight != 974403 || resp.ChainCurrent ||
		resp.PeerCount != 0 || resp.PeerBestHeight != 0 {

		t.Errorf("RPC backend: got %v", resp)
	}

	s = newNetworkTestServer(t, &fakeSPVChain{
		fakeChain:  fakeChain{height: 974403, current: true},
		peers:      3,
		peerHeight: 974410,
	})
	resp, err = s.Network(ctx, &pb.NetworkRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.BestHeight != 974403 || !resp.ChainCurrent ||
		resp.PeerCount != 3 || resp.PeerBestHeight != 974410 {

		t.Errorf("SPV backend: got %v", resp)
	}
}
