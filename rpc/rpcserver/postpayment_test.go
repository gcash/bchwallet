package rpcserver

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/pymtproto/payments"
	pb "github.com/gcash/bchwallet/rpc/walletrpc"
	"github.com/golang/protobuf/proto" //nolint:staticcheck // generated payments.pb.go still uses the v1 protobuf API.
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// recordingChain is a chain backend that records the transactions the wallet
// sends.
type recordingChain struct {
	fakeChain
	mtx  sync.Mutex
	sent []*wire.MsgTx
}

func (c *recordingChain) NotifyReceived([]bchutil.Address) error { return nil }

func (c *recordingChain) SendRawTransaction(tx *wire.MsgTx, _ bool) (*chainhash.Hash, error) {
	c.mtx.Lock()
	c.sent = append(c.sent, tx)
	c.mtx.Unlock()
	hash := tx.TxHash()
	return &hash, nil
}

func (c *recordingChain) sentTxs() []*wire.MsgTx {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	return append([]*wire.MsgTx(nil), c.sent...)
}

// TestPostPaymentRecordsTransaction ensures that once a merchant accepts a
// payment, PostPayment records its transaction in the wallet, which sends it to
// the network too, and that it records nothing when the merchant refuses it.
func TestPostPaymentRecordsTransaction(t *testing.T) {
	// A BIP70 merchant that accepts any payment.
	merchant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		payment := new(payments.Payment)
		if err := proto.Unmarshal(body, payment); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		memo := "Thanks"
		ack, err := proto.Marshal(&payments.PaymentACK{Payment: payment, Memo: &memo})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(ack)
	}))
	defer merchant.Close()

	chain := &recordingChain{}
	s := newNetworkTestServer(t, chain)
	a := newTestAddresses(t)
	ctx := context.Background()

	script, err := txscript.PayToAddrScript(a.pkh)
	if err != nil {
		t.Fatal(err)
	}
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&chainhash.Hash{1}, 0), []byte{0x51}))
	tx.AddTxOut(wire.NewTxOut(1000, script, wire.TokenData{}))
	var buf bytes.Buffer
	if err := tx.BchEncode(&buf, wire.ProtocolVersion, wire.BaseEncoding); err != nil {
		t.Fatal(err)
	}

	resp, err := s.PostPayment(ctx, &pb.PostPaymentRequest{
		PaymentUrl:   merchant.URL,
		Transactions: [][]byte{buf.Bytes()},
		RefundOutput: &pb.PostPaymentRequest_Output{Address: a.cashAddr, Amount: 1000},
	})
	if err != nil || resp.Memo != "Thanks" {
		t.Fatalf("got %v, %v", resp, err)
	}
	if sent := chain.sentTxs(); len(sent) != 1 || sent[0].TxHash() != tx.TxHash() {
		t.Fatalf("wallet sent %v, want the payment's transaction", sent)
	}

	// A JSON Payment Protocol payment needs no refund output, and as this
	// merchant does not take it, nothing more is recorded.
	_, err = s.PostPayment(ctx, &pb.PostPaymentRequest{
		PaymentUrl:   merchant.URL,
		Transactions: [][]byte{buf.Bytes()},
		Protocol:     pb.PaymentProtocol_JSON_PAYMENT_PROTOCOL_V2,
	})
	if err == nil || status.Code(err) == codes.InvalidArgument {
		t.Errorf("JSON payment to a BIP70 merchant: got %v", err)
	}
	if sent := chain.sentTxs(); len(sent) != 1 {
		t.Errorf("wallet sent %d transactions, want only the accepted one", len(sent))
	}
}
