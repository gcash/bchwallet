package pymtproto

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/pymtproto/payments"
	"github.com/golang/protobuf/proto" //nolint:staticcheck // generated payments.pb.go still uses the v1 protobuf API.
	"github.com/jarcoal/httpmock"
)

// testCert is a certificate and its private key.
type testCert struct {
	cert *x509.Certificate
	key  crypto.Signer
}

// newTestCert makes a certificate from template for key, signed by parent, or
// by key itself when parent is nil.
func newTestCert(t *testing.T, template *x509.Certificate, key crypto.Signer,
	parent *testCert) *testCert {

	t.Helper()

	issuer, issuerKey := template, key
	if parent != nil {
		issuer, issuerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer,
		key.Public(), issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCert{cert: cert, key: key}
}

func newECDSAKey(t *testing.T) crypto.Signer {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func newRSAKey(t *testing.T) crypto.Signer {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// caTemplate returns the template of a certificate authority's certificate.
func caTemplate(serial int64, name string) *x509.Certificate {
	return &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
}

// merchantTemplate returns the template of a merchant's certificate.
func merchantTemplate(serial int64, name string, dnsNames ...string) *x509.Certificate {
	return &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
}

// signedPaymentRequest returns a serialized payment request for network, or
// for none when it is empty, signed by merchant and carrying the certificates
// of chain.
func signedPaymentRequest(t *testing.T, merchant *testCert, chain []*testCert,
	network string) []byte {

	t.Helper()

	addr, err := bchutil.NewAddressPubKeyHash(make([]byte, 20),
		&chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatal(err)
	}
	return signedPaymentRequestPaying(t, merchant, chain, network, script)
}

// signedPaymentRequestPaying is signedPaymentRequest for a request whose one
// output pays script, with the edits made to its details.
func signedPaymentRequestPaying(t *testing.T, merchant *testCert,
	chain []*testCert, network string, script []byte,
	edits ...func(*payments.PaymentDetails)) []byte {

	t.Helper()

	amount := uint64(1000)
	now := uint64(time.Now().Unix())
	expires := now + 3600
	details := &payments.PaymentDetails{
		Outputs: []*payments.Output{{Amount: &amount, Script: script}},
		Time:    &now,
		Expires: &expires,
	}
	if network != "" {
		details.Network = &network
	}
	for _, edit := range edits {
		edit(details)
	}
	serializedDetails, err := proto.Marshal(details)
	if err != nil {
		t.Fatal(err)
	}

	certs := new(payments.X509Certificates)
	for _, c := range chain {
		certs.Certificate = append(certs.Certificate, c.cert.Raw)
	}
	pkiData, err := proto.Marshal(certs)
	if err != nil {
		t.Fatal(err)
	}

	pkiType := "x509+sha256"
	request := &payments.PaymentRequest{
		PkiType:                  &pkiType,
		PkiData:                  pkiData,
		SerializedPaymentDetails: serializedDetails,
		Signature:                []byte{},
	}
	unsigned, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(unsigned)
	request.Signature, err = merchant.key.Sign(rand.Reader, hash[:], crypto.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	serialized, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return serialized
}

// downloadPaymentRequest serves request and downloads it with a client for
// params that trusts only roots.
func downloadPaymentRequest(t *testing.T, request []byte, params *chaincfg.Params,
	roots ...*testCert) (*PaymentRequest, error) {

	t.Helper()

	client := &http.Client{}
	httpmock.ActivateNonDefault(client)
	defer httpmock.DeactivateAndReset()
	httpmock.RegisterResponder(http.MethodGet, "https://merchant.example/i/1",
		httpmock.NewBytesResponder(http.StatusOK, request))

	prClient := NewPaymentProtocolClient(params, nil)
	prClient.httpClient = client
	prClient.roots = x509.NewCertPool()
	for _, root := range roots {
		prClient.roots.AddCert(root.cert)
	}
	return prClient.DownloadBip0070PaymentRequest(context.Background(),
		"bitcoincash:?r=https://merchant.example/i/1")
}

// TestDownloadPaymentRequestCertificates ensures a payment request is accepted
// only when the merchant's certificate chains to a trusted certificate
// authority, through the intermediates the request includes, and when the
// request is for the client's network.
func TestDownloadPaymentRequestCertificates(t *testing.T) {
	// A trusted root, and an intermediate with an RSA key that certifies
	// merchants with ECDSA keys, so the algorithm of a merchant certificate's
	// signature is not the one of the merchant's signatures.
	root := newTestCert(t, caTemplate(1, "Test Root CA"), newECDSAKey(t), nil)
	intermediate := newTestCert(t, caTemplate(2, "Test Intermediate CA"),
		newRSAKey(t), root)
	merchant := newTestCert(t, merchantTemplate(3, "Example Store",
		"store.example"), newECDSAKey(t), intermediate)
	unnamed := newTestCert(t, merchantTemplate(4, "", "shop.example"),
		newECDSAKey(t), intermediate)

	// A certificate authority that anyone could make, with the trusted
	// root's name, and a certificate in a well-known merchant's name.
	forgedRoot := newTestCert(t, caTemplate(5, "Test Root CA"), newECDSAKey(t), nil)
	forged := newTestCert(t, merchantTemplate(6, "BitPay, Inc.", "bitpay.com"),
		newECDSAKey(t), forgedRoot)

	tests := []struct {
		name     string
		merchant *testCert
		chain    []*testCert
		network  string
		params   *chaincfg.Params
		payTo    string
		refusal  string // Part of the error when the request must be refused.
	}{
		{
			name:     "chain to a trusted root",
			merchant: merchant,
			chain:    []*testCert{merchant, intermediate},
			network:  "test",
			params:   &chaincfg.TestNet3Params,
			payTo:    "Example Store",
		},
		{
			name:     "chain that includes its root",
			merchant: merchant,
			chain:    []*testCert{merchant, intermediate, root},
			network:  "test",
			params:   &chaincfg.TestNet3Params,
			payTo:    "Example Store",
		},
		{
			name:     "certificate without a common name",
			merchant: unnamed,
			chain:    []*testCert{unnamed, intermediate},
			network:  "test",
			params:   &chaincfg.TestNet3Params,
			payTo:    "shop.example",
		},
		{
			name:     "mainnet request without a network",
			merchant: merchant,
			chain:    []*testCert{merchant, intermediate},
			params:   &chaincfg.MainNetParams,
			payTo:    "Example Store",
		},
		{
			name:     "root that the request supplies",
			merchant: forged,
			chain:    []*testCert{forged, forgedRoot},
			network:  "test",
			params:   &chaincfg.TestNet3Params,
			refusal:  "unknown authority",
		},
		{
			name:     "missing intermediate",
			merchant: merchant,
			chain:    []*testCert{merchant},
			network:  "test",
			params:   &chaincfg.TestNet3Params,
			refusal:  "unknown authority",
		},
		{
			name:     "mainnet request on testnet",
			merchant: merchant,
			chain:    []*testCert{merchant, intermediate},
			network:  "main",
			params:   &chaincfg.TestNet3Params,
			refusal:  "\"main\" network",
		},
		{
			name:     "testnet request on mainnet",
			merchant: merchant,
			chain:    []*testCert{merchant, intermediate},
			network:  "test",
			params:   &chaincfg.MainNetParams,
			refusal:  "\"test\" network",
		},
	}
	for _, test := range tests {
		request := signedPaymentRequest(t, test.merchant, test.chain, test.network)
		pr, err := downloadPaymentRequest(t, request, test.params, root)
		switch {
		case test.refusal != "" && err == nil:
			t.Errorf("%s: payment request accepted", test.name)
		case test.refusal != "" && !strings.Contains(err.Error(), test.refusal):
			t.Errorf("%s: refused with %q, want an error with %q",
				test.name, err, test.refusal)
		case test.refusal == "" && err != nil:
			t.Errorf("%s: %v", test.name, err)
		case test.refusal == "" && pr.PayToName != test.payTo:
			t.Errorf("%s: pay to name %q, want %q", test.name,
				pr.PayToName, test.payTo)
		}
	}
}

// TestDownloadPaymentRequestScripts ensures a BIP70 payment request is accepted
// only when each output pays one address by its hash, and that one whose
// output has no address, or addresses that pay something else, is refused
// rather than paid to the wrong address or crashing the client.
func TestDownloadPaymentRequestScripts(t *testing.T) {
	root := newTestCert(t, caTemplate(1, "Test Root CA"), newECDSAKey(t), nil)
	merchant := newTestCert(t, merchantTemplate(2, "Example Store",
		"store.example"), newECDSAKey(t), root)
	pub := make([]byte, 33)
	pub[0] = 0x02
	pub[32] = 0x01

	scriptHash, err := bchutil.NewAddressScriptHashFromHash(make([]byte, 20),
		&chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	p2sh, err := txscript.PayToAddrScript(scriptHash)
	if err != nil {
		t.Fatal(err)
	}
	opReturn, err := txscript.NullDataScript([]byte("memo"))
	if err != nil {
		t.Fatal(err)
	}
	multisig, err := txscript.NewScriptBuilder().AddOp(txscript.OP_1).
		AddData(pub).AddOp(txscript.OP_1).AddOp(txscript.OP_CHECKMULTISIG).
		Script()
	if err != nil {
		t.Fatal(err)
	}
	payToPubKey, err := txscript.NewScriptBuilder().AddData(pub).
		AddOp(txscript.OP_CHECKSIG).Script()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		script []byte
		ok     bool
	}{
		{"pay to script hash", p2sh, true},
		{"OP_RETURN", opReturn, false},
		{"bare multisig", multisig, false},
		{"pay to public key", payToPubKey, false},
	}
	for _, test := range tests {
		request := signedPaymentRequestPaying(t, merchant,
			[]*testCert{merchant}, "", test.script)
		pr, err := downloadPaymentRequest(t, request,
			&chaincfg.MainNetParams, root)
		switch {
		case test.ok && err != nil:
			t.Errorf("%s: %v", test.name, err)
		case test.ok && pr.Outputs[0].Address.EncodeAddress() !=
			scriptHash.EncodeAddress():
			t.Errorf("%s: pays %v", test.name, pr.Outputs[0].Address)
		case !test.ok && err == nil:
			t.Errorf("%s: payment request accepted, paying %v",
				test.name, pr.Outputs[0].Address)
		}
	}
}

// p2pkhScript returns the script that pays a mainnet address of zeros.
func p2pkhScript(t *testing.T) []byte {
	t.Helper()

	addr, err := bchutil.NewAddressPubKeyHash(make([]byte, 20),
		&chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	script, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatal(err)
	}
	return script
}

// largeMemo gives a payment request a memo that makes it larger than BIP70
// allows.
func largeMemo(details *payments.PaymentDetails) {
	memo := strings.Repeat("x", MaxPaymentRequestSize)
	details.Memo = &memo
}

// parsePaymentRequest checks request with a client for params that trusts
// only roots, as ParsePaymentRequest checks a request that the wallet has as
// a file.
func parsePaymentRequest(request []byte, params *chaincfg.Params,
	roots ...*testCert) (*PaymentRequest, error) {

	prClient := NewPaymentProtocolClient(params, nil)
	prClient.roots = x509.NewCertPool()
	for _, root := range roots {
		prClient.roots.AddCert(root.cert)
	}
	return prClient.ParseBip0070PaymentRequest(request)
}

// TestParsePaymentRequest ensures a payment request that the wallet has as a
// file gets the checks of a downloaded one, and is refused when it is larger
// than BIP70 allows.
func TestParsePaymentRequest(t *testing.T) {
	root := newTestCert(t, caTemplate(1, "Test Root CA"), newECDSAKey(t), nil)
	merchant := newTestCert(t, merchantTemplate(2, "Example Store",
		"store.example"), newECDSAKey(t), root)
	expiredTemplate := merchantTemplate(3, "Example Store", "store.example")
	expiredTemplate.NotBefore = time.Now().Add(-48 * time.Hour)
	expiredTemplate.NotAfter = time.Now().Add(-24 * time.Hour)
	expired := newTestCert(t, expiredTemplate, newECDSAKey(t), root)
	otherRoot := newTestCert(t, caTemplate(4, "Other Root CA"), newECDSAKey(t), nil)
	untrusted := newTestCert(t, merchantTemplate(5, "Example Store",
		"store.example"), newECDSAKey(t), otherRoot)
	script := p2pkhScript(t)

	tests := []struct {
		name    string
		request []byte
		refusal string // Part of the error when the request must be refused.
	}{
		{
			name: "request of a trusted merchant",
			request: signedPaymentRequestPaying(t, merchant,
				[]*testCert{merchant}, "test", script),
		},
		{
			name: "expired certificate",
			request: signedPaymentRequestPaying(t, expired,
				[]*testCert{expired}, "test", script),
			refusal: "certificate is expired",
		},
		{
			name: "certificate of an untrusted authority",
			request: signedPaymentRequestPaying(t, untrusted,
				[]*testCert{untrusted}, "test", script),
			refusal: "unknown authority",
		},
		{
			name: "mainnet request on testnet",
			request: signedPaymentRequestPaying(t, merchant,
				[]*testCert{merchant}, "main", script),
			refusal: "\"main\" network",
		},
		{
			name: "request larger than BIP70 allows",
			request: signedPaymentRequestPaying(t, merchant,
				[]*testCert{merchant}, "test", script, largeMemo),
			refusal: "larger than 50000 bytes",
		},
	}
	for _, test := range tests {
		pr, err := parsePaymentRequest(test.request,
			&chaincfg.TestNet3Params, root)
		switch {
		case test.refusal != "" && err == nil:
			t.Errorf("%s: payment request accepted", test.name)
		case test.refusal != "" && !strings.Contains(err.Error(), test.refusal):
			t.Errorf("%s: refused with %q, want an error with %q",
				test.name, err, test.refusal)
		case test.refusal == "" && err != nil:
			t.Errorf("%s: %v", test.name, err)
		case test.refusal == "" && pr.PayToName != "Example Store":
			t.Errorf("%s: pay to name %q", test.name, pr.PayToName)
		}
	}
}

// TestDownloadPaymentRequestSize ensures a downloaded payment request larger
// than BIP70 allows is refused, also when it is signed by a trusted merchant.
func TestDownloadPaymentRequestSize(t *testing.T) {
	root := newTestCert(t, caTemplate(1, "Test Root CA"), newECDSAKey(t), nil)
	merchant := newTestCert(t, merchantTemplate(2, "Example Store",
		"store.example"), newECDSAKey(t), root)
	request := signedPaymentRequestPaying(t, merchant, []*testCert{merchant},
		"", p2pkhScript(t), largeMemo)

	_, err := downloadPaymentRequest(t, request, &chaincfg.MainNetParams, root)
	if err == nil || !strings.Contains(err.Error(), "larger than 50000 bytes") {
		t.Fatalf("payment request of %d bytes: %v", len(request), err)
	}
}
