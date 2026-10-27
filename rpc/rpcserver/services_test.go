package rpcserver

import (
	"context"
	"testing"
	"time"

	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchwallet/netparams"
	pb "github.com/gcash/bchwallet/rpc/walletrpc"
	"github.com/gcash/bchwallet/wallet"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestServicesPerServer checks that each server gets services of its own, that
// its wallet service follows the wallet as a client closes it and opens one
// again, and that StopServices drops a server's services, so that a wallet
// loaded late can't start them again.
func TestServicesPerServer(t *testing.T) {
	const (
		walletName = "walletrpc.WalletService"
		loaderName = "walletrpc.WalletLoaderService"
	)
	ready := func(service string, srv interface{}) bool {
		return ServiceReadyFor(service, srv) == nil
	}
	first := newNetworkTestServer(t, nil).wallet.Load()
	second := newNetworkTestServer(t, nil).wallet.Load()
	loader := wallet.NewLoader(&chaincfg.MainNetParams, t.TempDir(), true, 250)

	a, b := grpc.NewServer(), grpc.NewServer()
	defer a.Stop()
	defer b.Stop()
	RegisterServices(a)
	RegisterServices(b)
	svcsA, svcsB := servicesOf(a), servicesOf(b)
	if svcsA == nil || svcsB == nil || svcsA.wallet == svcsB.wallet ||
		svcsA.loader == svcsB.loader {

		t.Fatal("the servers share their services")
	}
	if ready(walletName, svcsA.wallet) || ready(loaderName, svcsA.loader) {
		t.Fatal("services ready before they were started")
	}

	StartWalletLoaderService(a, loader, &netparams.MainNetParams)
	StartWalletService(a, first)
	if !ready(loaderName, svcsA.loader) || !ready(walletName, svcsA.wallet) ||
		svcsA.wallet.wallet.Load() != first {

		t.Fatal("services not ready with the first wallet")
	}
	if ready(walletName, svcsB.wallet) || ready(loaderName, svcsB.loader) {
		t.Fatal("starting one server's services started another's")
	}
	if ServiceReady(walletName) == nil {
		t.Error("ServiceReady didn't check the services registered last")
	}

	// A client closes the wallet and opens another.
	StopWalletService(a)
	if ready(walletName, svcsA.wallet) {
		t.Fatal("wallet service ready without a wallet")
	}
	StartWalletService(a, second)
	if !ready(walletName, svcsA.wallet) || svcsA.wallet.wallet.Load() != second {
		t.Fatal("wallet service doesn't serve the wallet opened again")
	}

	StopServices(a)
	StartWalletService(a, first)
	if ready(walletName, svcsA.wallet) || ready(loaderName, svcsA.loader) {
		t.Fatal("services ready after StopServices")
	}
	StopServices(b)
	if status.Code(ServiceReady(walletName)) != codes.FailedPrecondition {
		t.Error("ServiceReady found services after they were all stopped")
	}
}

// TestRescanWithoutChainClient checks that Rescan fails right away without a
// chain client, instead of waiting for one until the wallet stops, which held up
// the stop of the gRPC server, as it waits for its handlers.
func TestRescanWithoutChainClient(t *testing.T) {
	s := newNetworkTestServer(t, nil)

	done := make(chan error, 1)
	go func() {
		_, err := s.Rescan(context.Background(), &pb.RescanRequest{})
		done <- err
	}()
	select {
	case err := <-done:
		if status.Code(err) != codes.FailedPrecondition {
			t.Errorf("got %v, want FailedPrecondition", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Rescan waits for a chain client")
	}
}
