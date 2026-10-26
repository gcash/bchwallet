package pymtproto

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"fmt"
	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchwallet/pymtproto/payments"
	"github.com/golang/protobuf/proto" //nolint:staticcheck // generated payments.pb.go still uses the v1 protobuf API.
	"golang.org/x/net/proxy"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Protocol is a payment protocol.
type Protocol int

const (
	// ProtocolBIP70 is the BIP70 payment protocol.
	ProtocolBIP70 Protocol = iota

	// ProtocolJPPv2 is version 2 of BitPay's JSON Payment Protocol.
	ProtocolJPPv2
)

// PaymentRequest is returned by the DownloadPaymentRequest methods. It
// contains all the relevant information from the downloaded payment request.
type PaymentRequest struct {
	Protocol     Protocol
	PayToName    string
	Outputs      []Output
	Expires      time.Time
	Memo         string
	PaymentURL   string
	MerchantData []byte

	// RequiredFeeRate is the lowest fee rate, in satoshis per kB, that
	// the merchant accepts, or 0 when it does not say.
	RequiredFeeRate bchutil.Amount
}

// Output represents an address and amount to be paid.
type Output struct {
	Address bchutil.Address
	Amount  bchutil.Amount
}

// PaymentProtocolClient is used to download payment requests and make payments.
// We use an object for this to make testing a little easier.
type PaymentProtocolClient struct {
	httpClient           *http.Client
	params               *chaincfg.Params
	proxyDialer          proxy.Dialer
	skipExpirationChecks bool

	// roots are the certificate authorities that a payment request's
	// certificate must chain to.  Nil means the system's.
	roots *x509.CertPool

	// jppKeys are the keys trusted to sign JSON Payment Protocol
	// responses, by their identity.
	jppKeys map[string]jppKey
}

// NewPaymentProtocolClient returns a PaymentRequestDownloader that can be used to get the payment request.
func NewPaymentProtocolClient(params *chaincfg.Params, proxyDialer proxy.Dialer) *PaymentProtocolClient {
	// Use proxy on http connection if one is provided
	dialContext := (&net.Dialer{}).DialContext
	if proxyDialer != nil {
		dialContext = func(_ context.Context, network,
			addr string) (net.Conn, error) {

			return proxyDialer.Dial(network, addr)
		}
	}
	tbTransport := &http.Transport{DialContext: dialContext}
	client := &http.Client{Transport: tbTransport, Timeout: time.Minute}
	return &PaymentProtocolClient{
		httpClient:  client,
		params:      params,
		proxyDialer: proxyDialer,
		jppKeys:     bitPayKeys,
	}
}

// paymentRequestURL returns the URL of the payment request of a bitcoincash
// URI, its `r` parameter.
func paymentRequestURL(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	endpoint := u.Query().Get("r")
	if endpoint == "" {
		return "", errors.New("invalid bitcoin cash URI")
	}
	return endpoint, nil
}

// DownloadPaymentRequest downloads the payment request of a bitcoincash URI
// with the JSON Payment Protocol, and with BIP70 when the merchant does not
// offer the former or does not sign it with a key the client trusts.
func (c *PaymentProtocolClient) DownloadPaymentRequest(uri string) (*PaymentRequest, error) {
	pr, jppErr := c.DownloadJPPv2PaymentRequest(uri)
	if jppErr == nil {
		return pr, nil
	}
	pr, bip70Err := c.DownloadBip0070PaymentRequest(uri)
	if bip70Err != nil {
		return nil, fmt.Errorf("JSON payment protocol: %v; BIP70: %v",
			jppErr, bip70Err)
	}
	return pr, nil
}

// DownloadBip0070PaymentRequest will download a Bip70 (protobuf) payment request from
// the provided bitcoincash URI. Upon download it will validate the request is formatted
// correctly, is for the client's network and is signed with a valid X509 certificate.
// The cert must chain to a certificate authority in the OS's certificate store, through
// the intermediate certs the request includes. A PaymentRequest object with the relevant
// data extracted is returned.
func (c *PaymentProtocolClient) DownloadBip0070PaymentRequest(uri string) (*PaymentRequest, error) {
	endpoint, err := paymentRequestURL(uri)
	if err != nil {
		return nil, err
	}

	// Build GET request
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Add("Accept", "application/bitcoincash-paymentrequest")

	// Make the request
	resp, err := c.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http status not OK: %d", resp.StatusCode)
	}

	// Unmarshal payment request
	paymentRequestBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	paymentRequest := new(payments.PaymentRequest)
	if err = proto.Unmarshal(paymentRequestBytes, paymentRequest); err != nil {
		return nil, err
	}

	// We're only accepting `x509+sha256` certs. The alternatives are `none` which is insecure
	// and `x509+sha1` which is also insecure. So `x509+sha256` it is.
	switch paymentRequest.GetPkiType() {
	case "x509+sha256":
		break
	case "x509+sha1":
		return nil, errors.New("payment request PkiType x509+sha1 is insecure")
	default:
		return nil, errors.New("payment request PkiType unknown")
	}

	// Unmarshal the certificate object
	certificateProto := new(payments.X509Certificates)
	if err := proto.Unmarshal(paymentRequest.PkiData, certificateProto); err != nil {
		return nil, err
	}

	// Parse the certificates
	var certs []*x509.Certificate
	for _, certBytes := range certificateProto.Certificate {
		cert, err := x509.ParseCertificate(certBytes)
		if err != nil {
			return nil, err
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, errors.New("payment request has no certificate")
	}

	// If the certificate is expired or not valid yet we return and error
	if !c.skipExpirationChecks {
		if time.Now().After(certs[0].NotAfter) {
			return nil, errors.New("certificate is expired")
		}
	}
	if time.Now().Before(certs[0].NotBefore) {
		return nil, errors.New("certificate is not valid yet")
	}

	// Now make sure the merchant's certificate, the first one, chains to a
	// certificate authority that the system trusts.  The other certificates
	// are only intermediates: trusting one of them as a root would let anyone
	// sign a payment request in any merchant's name with a CA they made.
	intermediates := x509.NewCertPool()
	for _, cert := range certs[1:] {
		intermediates.AddCert(cert)
	}
	opts := x509.VerifyOptions{
		Roots:         c.roots,
		Intermediates: intermediates,
	}
	if c.skipExpirationChecks {
		opts.CurrentTime = certs[0].NotAfter.Add(-time.Minute)
	}
	if _, err := certs[0].Verify(opts); err != nil {
		return nil, err
	}

	// Verify the signature on the PaymentRequest object, which x509+sha256
	// makes over a SHA-256 hash with the key of the merchant's certificate.
	sigAlgorithm, err := sha256SignatureAlgorithm(certs[0].PublicKey)
	if err != nil {
		return nil, err
	}
	signature := paymentRequest.Signature
	paymentRequest.Signature = []byte{} // Zero out the signature for verification

	serializedPaymentRequest, err := proto.Marshal(paymentRequest)
	if err != nil {
		return nil, err
	}
	if err := certs[0].CheckSignature(sigAlgorithm, serializedPaymentRequest, signature); err != nil {
		return nil, err
	}

	// Parse the payment details and build the response
	paymentDetails := new(payments.PaymentDetails)
	if err := proto.Unmarshal(paymentRequest.SerializedPaymentDetails, paymentDetails); err != nil {
		return nil, err
	}

	// A payment request is for the main network or a test network.
	network := "main"
	if c.params.Net != wire.MainNet {
		network = "test"
	}
	if paymentDetails.GetNetwork() != network {
		return nil, fmt.Errorf("payment request is for the %q network, not %q",
			paymentDetails.GetNetwork(), network)
	}

	pr := &PaymentRequest{
		PayToName: payToName(certs[0]),
	}

	for _, out := range paymentDetails.Outputs {
		// We're going to return an error here if they ask us to pay an unparsable
		// address. This is kind of lame as we should be able to pay any script but
		// our gRPC API currently only works with addresses for convenience so we wont
		// be able to pay arbitrary scripts right now anyway.
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(out.Script, c.params)
		if err != nil {
			return nil, err
		}
		if out.Amount == nil {
			return nil, errors.New("nil payment amount")
		}
		output := Output{
			Address: addrs[0],
			Amount:  bchutil.Amount(int64(*out.Amount)),
		}
		pr.Outputs = append(pr.Outputs, output)
	}
	pr.Expires = time.Unix(int64(paymentDetails.GetExpires()), 0)

	if !c.skipExpirationChecks {
		if pr.Expires.Before(time.Now()) {
			return nil, errors.New("payment request is expired")
		}
	}

	pr.Memo = paymentDetails.GetMemo()
	pr.PaymentURL = paymentDetails.GetPaymentUrl()
	pr.MerchantData = paymentDetails.GetMerchantData()

	return pr, nil
}

// sha256SignatureAlgorithm returns the algorithm of a signature over a SHA-256
// hash made with a key of pub's type, as the x509+sha256 PKI type uses.
func sha256SignatureAlgorithm(pub any) (x509.SignatureAlgorithm, error) {
	switch pub.(type) {
	case *rsa.PublicKey:
		return x509.SHA256WithRSA, nil
	case *ecdsa.PublicKey:
		return x509.ECDSAWithSHA256, nil
	default:
		return x509.UnknownSignatureAlgorithm,
			fmt.Errorf("unsupported payment request certificate key %T", pub)
	}
}

// payToName returns the name of the merchant a certificate was issued to: its
// common name, or its first DNS name when it has none.
func payToName(cert *x509.Certificate) string {
	if cert.Subject.CommonName != "" || len(cert.DNSNames) == 0 {
		return cert.Subject.CommonName
	}
	return cert.DNSNames[0]
}

// Payment is an object that holds all information needed to POST a payment back
// to the merchant server. The protocol is the payment request's. All fields
// except memo are required, and BIP70 alone uses merchant data and the refund
// output.
type Payment struct {
	Protocol     Protocol
	PaymentURL   string
	MerchantData []byte
	Transactions []*wire.MsgTx
	RefundOutput Output
	Memo         string
}

// PostPayment sends a payment response back to the merchant's server. Any errors
// that are encountered in the process are returned along with an optional "memo"
// that the merchant can include in the ACK.
func (c *PaymentProtocolClient) PostPayment(payment *Payment) (memo string, err error) {
	if payment.Protocol == ProtocolJPPv2 {
		return c.postJPPv2Payment(payment)
	}

	// Build the payment protobuf object
	var transactions [][]byte
	for _, tx := range payment.Transactions {
		var buf bytes.Buffer
		if err := tx.BchEncode(&buf, 0, wire.BaseEncoding); err != nil {
			return "", err
		}
		transactions = append(transactions, buf.Bytes())
	}
	refundScript, err := txscript.PayToAddrScript(payment.RefundOutput.Address)
	if err != nil {
		return "", err
	}
	refundAmount := uint64(payment.RefundOutput.Amount.ToUnit(bchutil.AmountSatoshi))
	paymentProto := &payments.Payment{
		MerchantData: payment.MerchantData,
		Memo:         &payment.Memo,
		Transactions: transactions,
	}
	paymentProto.RefundTo = append(paymentProto.RefundTo, &payments.Output{
		Script: refundScript,
		Amount: &refundAmount,
	})

	// Marshal the protobuf
	serializedPayment, err := proto.Marshal(paymentProto)
	if err != nil {
		return "", err
	}

	// Build the POST request
	request, err := http.NewRequest(http.MethodPost, payment.PaymentURL, bytes.NewReader(serializedPayment))
	if err != nil {
		return "", err
	}

	request.Header.Add("Content-Type", "application/bitcoincash-payment")
	request.Header.Add("Accept", "application/bitcoincash-paymentack")

	// Make the request
	resp, err := c.httpClient.Do(request)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http status not OK: %d", resp.StatusCode)
	}

	serializedPaymentAck, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	paymentAck := new(payments.PaymentACK)
	if err := proto.Unmarshal(serializedPaymentAck, paymentAck); err != nil {
		return "", err
	}

	if paymentAck.Memo != nil {
		memo = *paymentAck.Memo
	}
	return memo, nil
}
