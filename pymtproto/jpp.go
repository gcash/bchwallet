package pymtproto

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/gcash/bchd/bchec"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
)

// This file implements version 2 of BitPay's JSON Payment Protocol, as
// https://github.com/bitpay/jsonPaymentProtocol/blob/master/v2/specification.md
// describes it and BitPay's own wallet speaks it.

const (
	// jppChain and jppCurrency name Bitcoin Cash in JSON Payment Protocol
	// messages.
	jppChain    = "BCH"
	jppCurrency = "BCH"

	// maxJPPFeeRate is the highest fee rate, in satoshis per byte, that a
	// payment request may require, the limit of BitPay's own wallet.
	maxJPPFeeRate = 10000

	// maxJPPResponseSize limits how much of a merchant's response is read.
	maxJPPResponseSize = 1 << 20
)

// jppKey is a key that a merchant signs its JSON Payment Protocol responses
// with.
type jppKey struct {
	owner     string   // Name of the merchant, shown to the user.
	networks  []string // "main" or "test"
	domains   []string // Hosts whose responses the key may sign.
	publicKey string   // Compressed, in hex.
}

// jppResponse is a merchant's response to a JSON Payment Protocol request.
type jppResponse struct {
	host   string // Host the request went to.
	header http.Header
	body   []byte
}

// jppPaymentOptions is the body of a payment options response.
type jppPaymentOptions struct {
	PaymentURL     string `json:"paymentUrl"`
	PaymentOptions []struct {
		Chain    string `json:"chain"`
		Currency string `json:"currency"`
		Network  string `json:"network"`
	} `json:"paymentOptions"`
}

// jppPaymentRequest is the body of a payment request response.
type jppPaymentRequest struct {
	Expires      time.Time `json:"expires"`
	Memo         string    `json:"memo"`
	PaymentURL   string    `json:"paymentUrl"`
	Chain        string    `json:"chain"`
	Network      string    `json:"network"`
	Instructions []struct {
		Type            string  `json:"type"`
		RequiredFeeRate float64 `json:"requiredFeeRate"`
		Outputs         []struct {
			Amount  int64  `json:"amount"`
			Address string `json:"address"`
		} `json:"outputs"`
	} `json:"instructions"`
}

// jppTransaction is a transaction in a payment verification or payment.
type jppTransaction struct {
	Tx           string `json:"tx"`
	WeightedSize int    `json:"weightedSize"`
}

// jppPayment is the body of a payment verification or payment.
type jppPayment struct {
	Chain        string           `json:"chain"`
	Currency     string           `json:"currency"`
	Transactions []jppTransaction `json:"transactions"`
}

// jppPaymentResponse is the body of the response to a payment verification or
// payment.
type jppPaymentResponse struct {
	Memo string `json:"memo"`
}

// jppNetwork returns the JSON Payment Protocol name of the client's network.
func (c *PaymentProtocolClient) jppNetwork() string {
	if c.params.Net == wire.MainNet {
		return "main"
	}
	return "test"
}

// jppDo makes a JSON Payment Protocol request, with body as JSON of
// contentType if it is not nil, and returns the response when its status is
// 200.  The response's signature is not checked.
func (c *PaymentProtocolClient) jppDo(ctx context.Context, method, rawURL,
	accept, contentType string, body interface{}) (*jppResponse, error) {

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	request, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("x-paypro-version", "2")
	if accept != "" {
		request.Header.Set("Accept", accept)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}

	resp, err := c.httpClient.Do(request)
	if err != nil {
		return nil, &stageError{stageConnect, err}
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxJPPResponseSize))
	if err != nil {
		return nil, &stageError{stageConnect, err}
	}
	if resp.StatusCode != http.StatusOK {
		// Errors come as plain text, which is worth passing on, but
		// keep a page that is not one short.
		msg := strings.TrimSpace(string(respBody))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, &stageError{stageStatus,
			fmt.Errorf("http status %d: %s", resp.StatusCode, msg)}
	}
	return &jppResponse{host: u.Hostname(), header: resp.Header, body: respBody}, nil
}

// verifyJPPResponse checks that a response comes from a merchant key that the
// client trusts for the host the request went to and the client's network,
// and returns the owner of the key.  The digest header must be the SHA-256 of
// the body, and the signature, by the key x-identity names, valid for it.
func (c *PaymentProtocolClient) verifyJPPResponse(resp *jppResponse) (string, error) {
	alg, digestHex, ok := strings.Cut(resp.header.Get("Digest"), "=")
	if !ok || !strings.EqualFold(alg, "SHA-256") {
		// The response of a server that doesn't speak the protocol, such
		// as a BIP70 payment request, has none.
		return "", &stageError{stageFormat,
			errors.New("response has no SHA-256 digest")}
	}
	digest, err := hex.DecodeString(digestHex)
	sum := sha256.Sum256(resp.body)
	if err != nil || !bytes.Equal(digest, sum[:]) {
		return "", errors.New("response digest does not match its body")
	}
	if !strings.EqualFold(resp.header.Get("X-Signature-Type"), "ecc") {
		return "", fmt.Errorf("unknown response signature type %q",
			resp.header.Get("X-Signature-Type"))
	}

	identity := resp.header.Get("X-Identity")
	key, ok := c.jppKeys[identity]
	if !ok {
		return "", fmt.Errorf("response signed by unknown key %q", identity)
	}
	if !slices.Contains(key.domains, resp.host) {
		return "", fmt.Errorf("key %s is not trusted for %s", identity, resp.host)
	}
	if !slices.Contains(key.networks, c.jppNetwork()) {
		return "", fmt.Errorf("key %s is not trusted for the %s network",
			identity, c.jppNetwork())
	}

	sigHex := resp.header.Get("Signature")
	if sigHex == "" {
		sigHex = resp.header.Get("X-Signature")
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != 64 {
		return "", errors.New("response has no valid signature")
	}
	keyBytes, err := hex.DecodeString(key.publicKey)
	if err != nil {
		return "", err
	}
	pubKey, err := bchec.ParsePubKey(keyBytes, bchec.S256())
	if err != nil {
		return "", err
	}
	signature := &bchec.Signature{
		R: new(big.Int).SetBytes(sig[:32]),
		S: new(big.Int).SetBytes(sig[32:]),
	}
	if !signature.Verify(digest, pubKey) {
		return "", errors.New("response signature is invalid")
	}
	return key.owner, nil
}

// DownloadJPPv2PaymentRequest downloads the payment request of a bitcoincash
// URI with BitPay's JSON Payment Protocol, version 2.  The merchant's
// responses must be signed by a key that the client trusts for the
// merchant's host and the client's network, such as BitPay's own.
func (c *PaymentProtocolClient) DownloadJPPv2PaymentRequest(ctx context.Context,
	uri string) (*PaymentRequest, error) {

	endpoint, err := paymentRequestURL(uri)
	if err != nil {
		return nil, err
	}
	network := c.jppNetwork()

	resp, err := c.jppDo(ctx, http.MethodGet, endpoint,
		"application/payment-options", "", nil)
	if err != nil {
		return nil, err
	}
	if _, err := c.verifyJPPResponse(resp); err != nil {
		return nil, err
	}
	var options jppPaymentOptions
	if err := json.Unmarshal(resp.body, &options); err != nil {
		return nil, fmt.Errorf("invalid payment options: %v", err)
	}
	accepted := false
	for _, option := range options.PaymentOptions {
		if strings.EqualFold(option.Chain, jppChain) &&
			strings.EqualFold(option.Currency, jppCurrency) &&
			option.Network == network {

			accepted = true
		}
	}
	if !accepted {
		return nil, fmt.Errorf("merchant does not accept BCH on the %s network",
			network)
	}

	paymentURL := options.PaymentURL
	if paymentURL == "" {
		paymentURL = endpoint
	}
	resp, err = c.jppDo(ctx, http.MethodPost, paymentURL, "",
		"application/payment-request",
		map[string]string{"chain": jppChain, "currency": jppCurrency})
	if err != nil {
		return nil, err
	}
	owner, err := c.verifyJPPResponse(resp)
	if err != nil {
		return nil, err
	}
	var request jppPaymentRequest
	if err := json.Unmarshal(resp.body, &request); err != nil {
		return nil, fmt.Errorf("invalid payment request: %v", err)
	}
	if !strings.EqualFold(request.Chain, jppChain) || request.Network != network {
		return nil, fmt.Errorf("payment request is for %s on the %q network, "+
			"not BCH on the %q network", request.Chain, request.Network, network)
	}
	if len(request.Instructions) != 1 || request.Instructions[0].Type != "transaction" {
		return nil, errors.New("payment request does not ask for one transaction")
	}
	instruction := request.Instructions[0]
	if instruction.RequiredFeeRate < 0 || instruction.RequiredFeeRate > maxJPPFeeRate {
		return nil, fmt.Errorf("payment request requires a fee rate of %v "+
			"satoshis per byte", instruction.RequiredFeeRate)
	}
	if !c.skipExpirationChecks && request.Expires.Before(time.Now()) {
		return nil, errors.New("payment request is expired")
	}
	if request.PaymentURL != "" {
		paymentURL = request.PaymentURL
	}

	pr := &PaymentRequest{
		Protocol:        ProtocolJPPv2,
		PayToName:       owner,
		Expires:         request.Expires,
		Memo:            request.Memo,
		PaymentURL:      paymentURL,
		RequiredFeeRate: bchutil.Amount(math.Ceil(instruction.RequiredFeeRate * 1000)),
	}
	for _, out := range instruction.Outputs {
		addr, err := bchutil.DecodeAddress(out.Address, c.params)
		if err != nil {
			return nil, fmt.Errorf("invalid payment request address %q: %v",
				out.Address, err)
		}
		if out.Amount <= 0 {
			return nil, fmt.Errorf("invalid payment request amount %d", out.Amount)
		}
		pr.Outputs = append(pr.Outputs, Output{
			Address: addr,
			Amount:  bchutil.Amount(out.Amount),
		})
	}
	if len(pr.Outputs) == 0 {
		return nil, errors.New("payment request has no outputs")
	}
	return pr, nil
}

// postJPPv2Payment sends a payment's transactions to the merchant, first
// unsigned for it to verify them, and only once it accepts them signed, and
// returns the merchant's memo.  The response to the signed transactions is
// not checked beyond its status, as the merchant has them by then.
func (c *PaymentProtocolClient) postJPPv2Payment(ctx context.Context,
	payment *Payment) (string, error) {

	unsigned := jppPayment{Chain: jppChain, Currency: jppCurrency}
	signed := jppPayment{Chain: jppChain, Currency: jppCurrency}
	for _, tx := range payment.Transactions {
		var buf bytes.Buffer
		if err := tx.BchEncode(&buf, 0, wire.BaseEncoding); err != nil {
			return "", err
		}
		size := buf.Len()
		signed.Transactions = append(signed.Transactions, jppTransaction{
			Tx:           hex.EncodeToString(buf.Bytes()),
			WeightedSize: size,
		})

		unsignedTx := tx.Copy()
		for _, in := range unsignedTx.TxIn {
			in.SignatureScript = nil
		}
		buf.Reset()
		if err := unsignedTx.BchEncode(&buf, 0, wire.BaseEncoding); err != nil {
			return "", err
		}
		unsigned.Transactions = append(unsigned.Transactions, jppTransaction{
			Tx:           hex.EncodeToString(buf.Bytes()),
			WeightedSize: size,
		})
	}

	resp, err := c.jppDo(ctx, http.MethodPost, payment.PaymentURL, "",
		"application/payment-verification", unsigned)
	if err != nil {
		return "", fmt.Errorf("merchant did not accept the payment: %w", err)
	}
	if _, err := c.verifyJPPResponse(resp); err != nil {
		return "", err
	}

	resp, err = c.jppDo(ctx, http.MethodPost, payment.PaymentURL, "",
		"application/payment", signed)
	if err != nil {
		return "", err
	}
	// The memo is optional, so a body without one is no error.
	var ack jppPaymentResponse
	_ = json.Unmarshal(resp.body, &ack)
	return ack.Memo, nil
}
