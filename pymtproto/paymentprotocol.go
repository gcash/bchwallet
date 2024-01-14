package pymtproto

import (
	"bytes"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/dcrlabs/bchwallet/pymtproto/payments"
	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/golang/protobuf/proto"
	"golang.org/x/net/proxy"
)

// PaymentRequest is returned by the DownloadBip0070PaymentRequest method. It
// contains all the relevant information from the downloaded payment request.
type PaymentRequest struct {
	PayToName    string
	Outputs      []Output
	Expires      time.Time
	Memo         string
	PaymentURL   string
	MerchantData []byte
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
}

// NewPaymentProtocolClient returns a PaymentRequestDownloader that can be used to get the payment request.
func NewPaymentProtocolClient(params *chaincfg.Params, proxyDialer proxy.Dialer) *PaymentProtocolClient {
	// Use proxy on http connection if one is provided
	dial := net.Dial
	if proxyDialer != nil {
		dial = proxyDialer.Dial
	}
	tbTransport := &http.Transport{Dial: dial}
	client := &http.Client{Transport: tbTransport, Timeout: time.Minute}
	return &PaymentProtocolClient{
		httpClient:  client,
		params:      params,
		proxyDialer: proxyDialer,
	}
}

// DownloadBip0070PaymentRequest will download a Bip70 (protobuf) payment request from
// the provided bitcoincash URI. Upon download it will validate the request is formatted
// correctly and signed with a valid X509 certificate. The cert will be checked against
// the OS's certificate store. A PaymentRequest object with the relevant data extracted
// is returned.
func (c *PaymentProtocolClient) DownloadBip0070PaymentRequest(uri string) (*PaymentRequest, error) {
	// Extract the `r` parameter from the URI
	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}
	endpoint := u.Query().Get("r")
	if endpoint == "" {
		return nil, errors.New("invalid bitcoin cash URI")
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
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http status not OK: %d", resp.StatusCode)
	}

	// Unmarshal payment request
	paymentRequestBytes, err := ioutil.ReadAll(resp.Body)
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
	if len(certs) < 2 {
		return nil, errors.New("invalid number of certs")
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

	// Now make sure the cert is signed by a valid certificate authority
	roots := x509.NewCertPool()
	roots.AddCert(certs[1])

	opts := x509.VerifyOptions{
		Roots: roots,
	}
	if c.skipExpirationChecks {
		opts.CurrentTime = certs[0].NotAfter.Add(-time.Minute)
	}
	if _, err := certs[0].Verify(opts); err != nil {
		return nil, err
	}

	// Verify the signature on the PaymentRequest object
	signature := paymentRequest.Signature
	paymentRequest.Signature = []byte{} // Zero out the signature for verification

	serializedPaymentRequest, err := proto.Marshal(paymentRequest)
	if err != nil {
		return nil, err
	}
	if err := certs[0].CheckSignature(certs[0].SignatureAlgorithm, serializedPaymentRequest, signature); err != nil {
		return nil, err
	}

	// Parse the payment details and build the response
	paymentDetails := new(payments.PaymentDetails)
	if err := proto.Unmarshal(paymentRequest.SerializedPaymentDetails, paymentDetails); err != nil {
		return nil, err
	}

	pr := &PaymentRequest{
		PayToName: certs[0].Subject.CommonName,
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

// Payment is an object that holds all information needed to POST a payment back
// to the merchant server. All fields except memo are required.
type Payment struct {
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

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http status not OK: %d", resp.StatusCode)
	}

	serializedPaymentAck, err := ioutil.ReadAll(resp.Body)
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
