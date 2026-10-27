package rpcserver

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
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

// TestPostPaymentRemembersJPP checks that PostPayment without a protocol, as a
// client that predates the field sends it, pays a request that
// DownloadPaymentRequest returned as a JSON Payment Protocol one with that
// protocol, until the request expires, and pays others with BIP70.
func TestPostPaymentRemembersJPP(t *testing.T) {
	var mtx sync.Mutex
	var contentTypes []string
	merchant := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			mtx.Lock()
			contentTypes = append(contentTypes, r.Header.Get("Content-Type"))
			mtx.Unlock()
			http.Error(w, "refused", http.StatusBadRequest)
		}))
	defer merchant.Close()

	s := newNetworkTestServer(t, &recordingChain{})
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&chainhash.Hash{1}, 0), []byte{0x51}))
	tx.AddTxOut(wire.NewTxOut(1000, []byte{txscript.OP_TRUE}, wire.TokenData{}))
	var buf bytes.Buffer
	if err := tx.BchEncode(&buf, wire.ProtocolVersion, wire.BaseEncoding); err != nil {
		t.Fatal(err)
	}
	post := func(paymentURL string) error {
		_, err := s.PostPayment(context.Background(), &pb.PostPaymentRequest{
			PaymentUrl:   paymentURL,
			Transactions: [][]byte{buf.Bytes()},
		})
		return err
	}

	jpp := merchant.URL + "/jpp"
	expired := merchant.URL + "/expired"
	s.rememberJPPRequest(jpp, time.Now().Add(time.Hour))
	s.rememberJPPRequest(expired, time.Now().Add(-time.Minute))

	// Without a refund output, a BIP70 payment is refused before it reaches
	// the merchant.
	for _, paymentURL := range []string{merchant.URL + "/bip70", expired} {
		if err := post(paymentURL); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: got %v, want BIP70's InvalidArgument", paymentURL, err)
		}
	}
	if err := post(jpp); err == nil || status.Code(err) == codes.InvalidArgument {
		t.Errorf("JSON Payment Protocol request: got %v, want the merchant's "+
			"refusal", err)
	}
	mtx.Lock()
	defer mtx.Unlock()
	if len(contentTypes) != 1 || contentTypes[0] != "application/payment-verification" {
		t.Errorf("merchant got %v, want one payment verification", contentTypes)
	}
}
