package pymtproto

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gcash/bchd/bchec"
	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
)

// testMerchant is a JSON Payment Protocol merchant for tests, which signs its
// responses with key, and can serve a BIP70 payment request too.
type testMerchant struct {
	key      *bchec.PrivateKey
	identity string
	network  string
	feeRate  float64
	expires  time.Time
	address  string
	amount   int64

	// tamper breaks the signed responses: "digest", "signature" or
	// "identity".
	tamper string
	// noJPP makes the merchant refuse the JSON Payment Protocol.
	noJPP bool
	// bip70 is the BIP70 payment request to serve, if any.
	bip70 []byte
	// verifyStatus is the status of payment verifications, 200 when 0.
	verifyStatus int

	url string

	mtx      sync.Mutex
	verified []jppTransaction
	paid     []jppTransaction
}

func (m *testMerchant) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	accept := r.Header.Get("Accept")
	contentType := r.Header.Get("Content-Type")
	switch {
	case r.Method == http.MethodGet && accept == "application/bitcoincash-paymentrequest":
		if m.bip70 == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write(m.bip70)

	case r.Header.Get("x-paypro-version") != "2" || m.noJPP:
		http.Error(w, "unsupported", http.StatusBadRequest)

	case r.Method == http.MethodGet && accept == "application/payment-options":
		m.respond(w, map[string]interface{}{
			"paymentUrl": m.url,
			"paymentId":  "1",
			"expires":    m.expires,
			"paymentOptions": []map[string]interface{}{
				{"chain": "BCH", "currency": "BCH", "network": m.network},
			},
		})

	case r.Method == http.MethodPost && contentType == "application/payment-request":
		m.respond(w, map[string]interface{}{
			"paymentUrl": m.url,
			"paymentId":  "1",
			"expires":    m.expires,
			"memo":       "Pay the test merchant",
			"chain":      "BCH",
			"network":    m.network,
			"instructions": []map[string]interface{}{{
				"type":            "transaction",
				"requiredFeeRate": m.feeRate,
				"outputs": []map[string]interface{}{
					{"amount": m.amount, "address": m.address},
				},
			}},
		})

	case r.Method == http.MethodPost && contentType == "application/payment-verification":
		var payment jppPayment
		_ = json.NewDecoder(r.Body).Decode(&payment)
		m.mtx.Lock()
		m.verified = append(m.verified, payment.Transactions...)
		m.mtx.Unlock()
		if m.verifyStatus != 0 {
			http.Error(w, "Transaction fee is below the minimum", m.verifyStatus)
			return
		}
		m.respond(w, map[string]interface{}{"memo": "Payment appears valid"})

	case r.Method == http.MethodPost && contentType == "application/payment":
		var payment jppPayment
		_ = json.NewDecoder(r.Body).Decode(&payment)
		m.mtx.Lock()
		m.paid = append(m.paid, payment.Transactions...)
		m.mtx.Unlock()
		m.respond(w, map[string]interface{}{"memo": "Transaction received"})

	default:
		http.Error(w, "bad request", http.StatusBadRequest)
	}
}

// respond writes v as a JSON response signed as BitPay signs them.
func (m *testMerchant) respond(w http.ResponseWriter, v interface{}) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	digest := sha256.Sum256(body)
	signed := digest
	if m.tamper == "signature" {
		signed = sha256.Sum256([]byte("another body"))
	}
	sig, err := m.key.SignECDSA(signed[:])
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var rs [64]byte
	sig.R.FillBytes(rs[:32])
	sig.S.FillBytes(rs[32:])
	if m.tamper == "digest" {
		digest = sha256.Sum256([]byte("another body"))
	}
	identity := m.identity
	if m.tamper == "identity" {
		identity = "1BitcoinEaterAddressDontSendf59kuE"
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Digest", "SHA-256="+hex.EncodeToString(digest[:]))
	w.Header().Set("X-Signature-Type", "ECC")
	w.Header().Set("X-Identity", identity)
	w.Header().Set("Signature", hex.EncodeToString(rs[:]))
	_, _ = w.Write(body)
}

// newTestMerchant starts a JSON Payment Protocol merchant over TLS, and
// returns it with a mainnet client that trusts its key for its host.
func newTestMerchant(t *testing.T) (*testMerchant, *PaymentProtocolClient) {
	t.Helper()

	key, err := bchec.NewPrivateKey(bchec.S256())
	if err != nil {
		t.Fatal(err)
	}
	pub := key.PubKey().SerializeCompressed()
	identity, err := bchutil.NewLegacyAddressPubKeyHash(bchutil.Hash160(pub),
		&chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	m := &testMerchant{
		key:      key,
		identity: identity.EncodeAddress(),
		network:  "main",
		feeRate:  1,
		expires:  time.Now().Add(time.Hour),
		address:  "bitcoincash:" + testPayee(t).EncodeAddress(),
		amount:   239200,
	}
	server := httptest.NewTLSServer(m)
	t.Cleanup(server.Close)
	m.url = server.URL + "/i/1"

	client := NewPaymentProtocolClient(&chaincfg.MainNetParams, nil)
	client.httpClient = server.Client()
	client.jppKeys = map[string]jppKey{
		m.identity: {
			owner:     "Test Merchant",
			networks:  []string{"main"},
			domains:   []string{"127.0.0.1"},
			publicKey: hex.EncodeToString(pub),
		},
	}
	return m, client
}

// testPayee is the address the test merchant asks to be paid to.
func testPayee(t *testing.T) *bchutil.AddressPubKeyHash {
	t.Helper()

	addr, err := bchutil.NewAddressPubKeyHash(bytes.Repeat([]byte{0x42}, 20),
		&chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	return addr
}

// testSignedTx returns a transaction paying the test merchant, with a
// signature script that stands in for a signature.
func testSignedTx(t *testing.T) *wire.MsgTx {
	t.Helper()

	script, err := txscript.PayToAddrScript(testPayee(t))
	if err != nil {
		t.Fatal(err)
	}
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&chainhash.Hash{1}, 0),
		[]byte{0x01, 0x02, 0x03}))
	tx.AddTxOut(wire.NewTxOut(239200, script, wire.TokenData{}))
	return tx
}

func hexTx(t *testing.T, tx *wire.MsgTx) (string, int) {
	t.Helper()

	var buf bytes.Buffer
	if err := tx.BchEncode(&buf, 0, wire.BaseEncoding); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buf.Bytes()), buf.Len()
}

// TestJPPv2Payment pays a JSON Payment Protocol request: the merchant gets
// the transaction unsigned to verify, and then signed.
func TestJPPv2Payment(t *testing.T) {
	m, client := newTestMerchant(t)

	pr, err := client.DownloadPaymentRequest(context.Background(),
		"bitcoincash:?r="+m.url)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Protocol != ProtocolJPPv2 || pr.PayToName != "Test Merchant" ||
		pr.PaymentURL != m.url || pr.Memo != "Pay the test merchant" ||
		pr.RequiredFeeRate != 1000 {

		t.Fatalf("got payment request %+v", pr)
	}
	if len(pr.Outputs) != 1 || pr.Outputs[0].Amount != 239200 ||
		pr.Outputs[0].Address.EncodeAddress() != testPayee(t).EncodeAddress() {

		t.Fatalf("got outputs %+v", pr.Outputs)
	}

	tx := testSignedTx(t)
	memo, err := client.PostPayment(context.Background(), &Payment{
		Protocol:     ProtocolJPPv2,
		PaymentURL:   pr.PaymentURL,
		Transactions: []*wire.MsgTx{tx},
	})
	if err != nil || memo != "Transaction received" {
		t.Fatalf("got %q, %v", memo, err)
	}

	signedHex, signedSize := hexTx(t, tx)
	unsignedTx := tx.Copy()
	unsignedTx.TxIn[0].SignatureScript = nil
	unsignedHex, _ := hexTx(t, unsignedTx)
	m.mtx.Lock()
	defer m.mtx.Unlock()
	if len(m.verified) != 1 || m.verified[0].Tx != unsignedHex ||
		m.verified[0].WeightedSize != signedSize {

		t.Errorf("merchant verified %+v, want the unsigned transaction %s "+
			"weighing %d", m.verified, unsignedHex, signedSize)
	}
	if len(m.paid) != 1 || m.paid[0].Tx != signedHex {
		t.Errorf("merchant was paid %+v, want %s", m.paid, signedHex)
	}
}

// TestJPPv2RefusedRequests ensures a payment request is refused unless the
// merchant signs it with a key trusted for its host and the network, and asks
// for an acceptable payment.
func TestJPPv2RefusedRequests(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testMerchant, *PaymentProtocolClient)
		reason string
	}{
		{"digest of another body", func(m *testMerchant, _ *PaymentProtocolClient) {
			m.tamper = "digest"
		}, "digest does not match"},
		{"signature of another body", func(m *testMerchant, _ *PaymentProtocolClient) {
			m.tamper = "signature"
		}, "signature is invalid"},
		{"unknown key", func(m *testMerchant, _ *PaymentProtocolClient) {
			m.tamper = "identity"
		}, "unknown key"},
		{"key of another domain", func(m *testMerchant, c *PaymentProtocolClient) {
			key := c.jppKeys[m.identity]
			key.domains = []string{"bitpay.com"}
			c.jppKeys[m.identity] = key
		}, "not trusted for 127.0.0.1"},
		{"testnet merchant", func(m *testMerchant, _ *PaymentProtocolClient) {
			m.network = "test"
		}, "does not accept BCH on the main network"},
		{"fee rate too high", func(m *testMerchant, _ *PaymentProtocolClient) {
			m.feeRate = maxJPPFeeRate + 1
		}, "fee rate"},
		{"expired", func(m *testMerchant, _ *PaymentProtocolClient) {
			m.expires = time.Now().Add(-time.Minute)
		}, "expired"},
	}
	for _, test := range tests {
		m, client := newTestMerchant(t)
		test.change(m, client)
		_, err := client.DownloadJPPv2PaymentRequest(context.Background(),
			"bitcoincash:?r="+m.url)
		if err == nil || !strings.Contains(err.Error(), test.reason) {
			t.Errorf("%s: got %v, want an error with %q", test.name, err,
				test.reason)
		}
	}
}

// TestJPPv2VerificationRefused ensures a payment that the merchant refuses to
// verify is not sent signed.
func TestJPPv2VerificationRefused(t *testing.T) {
	m, client := newTestMerchant(t)
	m.verifyStatus = http.StatusUnprocessableEntity

	_, err := client.PostPayment(context.Background(), &Payment{
		Protocol:     ProtocolJPPv2,
		PaymentURL:   m.url,
		Transactions: []*wire.MsgTx{testSignedTx(t)},
	})
	if err == nil || !strings.Contains(err.Error(), "Transaction fee is below the minimum") {
		t.Errorf("got %v, want the merchant's refusal", err)
	}
	m.mtx.Lock()
	defer m.mtx.Unlock()
	if len(m.paid) != 0 {
		t.Errorf("payment sent although the merchant refused it: %+v", m.paid)
	}
}

// TestPaymentRequestFallsBackToBIP70 ensures a merchant without the JSON
// Payment Protocol is paid with BIP70, and that both failures are reported
// when it has neither.
func TestPaymentRequestFallsBackToBIP70(t *testing.T) {
	m, client := newTestMerchant(t)
	m.noJPP = true
	uri := "bitcoincash:?r=" + m.url

	_, err := client.DownloadPaymentRequest(context.Background(), uri)
	if err == nil || !strings.Contains(err.Error(), "JSON payment protocol") ||
		!strings.Contains(err.Error(), "BIP70") {

		t.Errorf("without either protocol: got %v", err)
	}

	root := newTestCert(t, caTemplate(1, "Test Root CA"), newECDSAKey(t), nil)
	merchant := newTestCert(t, merchantTemplate(2, "Example Store",
		"store.example"), newECDSAKey(t), root)
	m.bip70 = signedPaymentRequest(t, merchant, []*testCert{merchant}, "")
	client.roots = x509.NewCertPool()
	client.roots.AddCert(root.cert)

	pr, err := client.DownloadPaymentRequest(context.Background(), uri)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Protocol != ProtocolBIP70 || pr.PayToName != "Example Store" {
		t.Errorf("got payment request %+v", pr)
	}
}

// TestBitPayKeys checks that each of BitPay's keys has the identity of its
// public key, the address that x-identity names.
func TestBitPayKeys(t *testing.T) {
	if len(bitPayKeys) == 0 {
		t.Fatal("no BitPay keys")
	}
	for identity, key := range bitPayKeys {
		pub, err := hex.DecodeString(key.publicKey)
		if err != nil {
			t.Fatalf("%s: %v", identity, err)
		}
		if _, err := bchec.ParsePubKey(pub, bchec.S256()); err != nil {
			t.Fatalf("%s: %v", identity, err)
		}
		params := &chaincfg.MainNetParams
		if len(key.networks) == 1 && key.networks[0] == "test" {
			params = &chaincfg.TestNet3Params
		}
		addr, err := bchutil.NewLegacyAddressPubKeyHash(bchutil.Hash160(pub), params)
		if err != nil {
			t.Fatal(err)
		}
		if addr.EncodeAddress() != identity {
			t.Errorf("key %s has the identity %s", identity, addr.EncodeAddress())
		}
		if key.owner == "" || len(key.domains) == 0 || len(key.networks) == 0 {
			t.Errorf("key %s is incomplete: %+v", identity, key)
		}
	}
}

// TestPaymentProtocolCancel ensures a request to a merchant that never answers
// ends once its context is done, which is how stopping the gRPC server ends a
// call that waits on a merchant.
func TestPaymentProtocolCancel(t *testing.T) {
	// A handler that doesn't read a POST's body doesn't see the client go
	// away, so the server's Close waits for release.
	release := make(chan struct{})
	stuck := httptest.NewTLSServer(http.HandlerFunc(
		func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
	t.Cleanup(func() {
		close(release)
		stuck.Close()
	})
	client := NewPaymentProtocolClient(&chaincfg.MainNetParams, nil)
	client.httpClient = stuck.Client()

	calls := []struct {
		name string
		call func(ctx context.Context) error
	}{
		{"DownloadPaymentRequest", func(ctx context.Context) error {
			_, err := client.DownloadPaymentRequest(ctx,
				"bitcoincash:?r="+stuck.URL)
			return err
		}},
		{"PostPayment with BIP70", func(ctx context.Context) error {
			_, err := client.PostPayment(ctx, &Payment{
				Protocol:     ProtocolBIP70,
				PaymentURL:   stuck.URL,
				Transactions: []*wire.MsgTx{testSignedTx(t)},
				RefundOutput: Output{Address: testPayee(t), Amount: 1},
			})
			return err
		}},
		{"PostPayment with the JSON Payment Protocol",
			func(ctx context.Context) error {
				_, err := client.PostPayment(ctx, &Payment{
					Protocol:     ProtocolJPPv2,
					PaymentURL:   stuck.URL,
					Transactions: []*wire.MsgTx{testSignedTx(t)},
				})
				return err
			}},
	}
	for _, c := range calls {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(100*time.Millisecond, cancel)
		done := make(chan error, 1)
		go func() { done <- c.call(ctx) }()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("%s: got %v, want %v", c.name, err,
					context.Canceled)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s did not end when its context was cancelled",
				c.name)
		}
		cancel()
	}
}
