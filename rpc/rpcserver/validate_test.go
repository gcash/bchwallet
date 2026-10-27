package rpcserver

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/txscript"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/gcash/bchutil/hdkeychain"
	pb "github.com/gcash/bchwallet/rpc/walletrpc"
	"github.com/gcash/bchwallet/wallet"
	"github.com/gcash/bchwallet/wallet/txrules"
	"github.com/gcash/bchwallet/wallet/txsizes"
	_ "github.com/gcash/bchwallet/walletdb/bdb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// testAddresses are addresses of one key hash, in every form a client might
// send, for mainnet unless named otherwise.
type testAddresses struct {
	pkh         *bchutil.AddressPubKeyHash
	cashAddr    string
	tokenAware  string
	p2sh32      string
	legacy      string
	slp         string
	testnetCash string
	testnetLeg  string
	pubKey      string
}

func newTestAddresses(t *testing.T) *testAddresses {
	t.Helper()

	hash := bytes.Repeat([]byte{0x42}, 20)
	main := &chaincfg.MainNetParams
	pkh, err := bchutil.NewAddressPubKeyHash(hash, main)
	if err != nil {
		t.Fatal(err)
	}
	p2sh32, err := bchutil.NewAddressScriptHash32FromHash(
		bytes.Repeat([]byte{0x42}, 32), main)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := bchutil.NewLegacyAddressPubKeyHash(hash, main)
	if err != nil {
		t.Fatal(err)
	}
	slp, err := bchutil.NewSlpAddressPubKeyHash(hash, main)
	if err != nil {
		t.Fatal(err)
	}
	testnetCash, err := bchutil.NewAddressPubKeyHash(hash, &chaincfg.TestNet3Params)
	if err != nil {
		t.Fatal(err)
	}
	testnetLeg, err := bchutil.NewLegacyAddressPubKeyHash(hash, &chaincfg.TestNet3Params)
	if err != nil {
		t.Fatal(err)
	}
	return &testAddresses{
		pkh:         pkh,
		cashAddr:    "bitcoincash:" + pkh.EncodeAddress(),
		tokenAware:  "bitcoincash:" + pkh.TokenAware().CashAddress(),
		p2sh32:      "bitcoincash:" + p2sh32.EncodeAddress(),
		legacy:      legacy.EncodeAddress(),
		slp:         "simpleledger:" + slp.EncodeAddress(),
		testnetCash: "bchtest:" + testnetCash.EncodeAddress(),
		testnetLeg:  testnetLeg.EncodeAddress(),
		pubKey:      "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
	}
}

func TestDecodeAddress(t *testing.T) {
	a := newTestAddresses(t)
	tests := []struct {
		address string
		valid   bool
	}{
		{a.cashAddr, true},
		{a.tokenAware, true},
		{a.p2sh32, true},
		{a.legacy, true},
		{a.slp, true},
		{a.testnetCash, false},
		{a.testnetLeg, false},
		{a.pubKey, false},
		{"bitcoincash:qqqqqqqq", false},
	}
	for _, test := range tests {
		_, err := decodeAddress(test.address, &chaincfg.MainNetParams)
		if test.valid && err != nil {
			t.Errorf("%s: %v", test.address, err)
		}
		if !test.valid && status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: got %v, want InvalidArgument", test.address, err)
		}
	}
}

func TestFeeRate(t *testing.T) {
	tests := []struct {
		satPerKb uint32
		rate     bchutil.Amount
		code     codes.Code
	}{
		{0, txrules.DefaultRelayFeePerKb, codes.OK},
		{999, 0, codes.InvalidArgument},
		{1000, 1000, codes.OK},
		{5000, 5000, codes.OK},
	}
	for _, test := range tests {
		rate, err := feeRate(test.satPerKb)
		if status.Code(err) != test.code || rate != test.rate {
			t.Errorf("%d: got %v, %v, want %v, %v", test.satPerKb, rate,
				err, test.rate, test.code)
		}
	}
}

func TestRequestOutputs(t *testing.T) {
	a := newTestAddresses(t)
	script, err := txscript.PayToAddrScript(a.pkh)
	if err != nil {
		t.Fatal(err)
	}

	outputs, nfts, err := requestOutputs([]*pb.CreateTransactionRequest_Output{
		{Address: a.cashAddr, Amount: 546},
	}, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatalf("output at the dust limit: %v", err)
	}
	if len(outputs) != 1 || outputs[0].Value != 546 ||
		!bytes.Equal(outputs[0].PkScript, script) || len(nfts) != 1 ||
		nfts[0] != nil {

		t.Fatalf("got outputs %v, want 546 satoshis to %x", outputs, script)
	}

	invalid := []struct {
		name    string
		outputs []*pb.CreateTransactionRequest_Output
	}{
		{"no outputs", nil},
		{"below the dust limit", []*pb.CreateTransactionRequest_Output{
			{Address: a.cashAddr, Amount: 545}}},
		{"zero", []*pb.CreateTransactionRequest_Output{
			{Address: a.cashAddr, Amount: 0}}},
		{"negative", []*pb.CreateTransactionRequest_Output{
			{Address: a.cashAddr, Amount: -1}}},
		{"above the money supply", []*pb.CreateTransactionRequest_Output{
			{Address: a.cashAddr, Amount: bchutil.MaxSatoshi + 1}}},
		{"public key", []*pb.CreateTransactionRequest_Output{
			{Address: a.pubKey, Amount: 1000}}},
		{"testnet address", []*pb.CreateTransactionRequest_Output{
			{Address: a.cashAddr, Amount: 1000},
			{Address: a.testnetLeg, Amount: 1000}}},
	}
	for _, test := range invalid {
		_, _, err := requestOutputs(test.outputs, &chaincfg.MainNetParams)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: got %v, want InvalidArgument", test.name, err)
		}
	}
}

func TestSweepOutput(t *testing.T) {
	a := newTestAddresses(t)
	script, err := txscript.PayToAddrScript(a.pkh)
	if err != nil {
		t.Fatal(err)
	}
	size := txsizes.EstimateSerializeSize(1,
		[]*wire.TxOut{wire.NewTxOut(0, script, wire.TokenData{})}, false)
	fee := txrules.FeeForSerializeSize(txrules.DefaultRelayFeePerKb, size)

	out, gotFee, err := sweepOutput(script, 1, fee+546,
		txrules.DefaultRelayFeePerKb)
	if err != nil {
		t.Fatalf("sweep leaving the dust limit: %v", err)
	}
	if out.Value != 546 || gotFee != fee || !bytes.Equal(out.PkScript, script) {
		t.Fatalf("got %v satoshis and a fee of %v, want 546 and %v",
			out.Value, gotFee, fee)
	}

	invalid := []struct {
		name      string
		numInputs int
		total     bchutil.Amount
		rate      bchutil.Amount
		code      codes.Code
	}{
		{"no inputs", 0, 100000, txrules.DefaultRelayFeePerKb, codes.FailedPrecondition},
		{"below the dust limit", 1, fee + 545, txrules.DefaultRelayFeePerKb, codes.FailedPrecondition},
		{"less than the fee", 1, fee - 1, txrules.DefaultRelayFeePerKb, codes.FailedPrecondition},
		{"fee above the maximum", 1, bchutil.SatoshiPerBitcoin, 100000000, codes.InvalidArgument},
	}
	for _, test := range invalid {
		_, _, err := sweepOutput(script, test.numInputs, test.total, test.rate)
		if status.Code(err) != test.code {
			t.Errorf("%s: got %v, want %v", test.name, err, test.code)
		}
	}
}

// TestHandlerValidation checks that the handlers refuse invalid requests with
// the right codes, on a mainnet wallet without funds or a chain backend.
func TestHandlerValidation(t *testing.T) {
	a := newTestAddresses(t)
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
	defer func() {
		if err := loader.UnloadWallet(); err != nil {
			t.Error(err)
		}
	}()
	s := &walletServer{wallet: w}
	ctx := context.Background()

	for address, valid := range map[string]bool{
		a.cashAddr:   true,
		a.legacy:     true,
		a.testnetLeg: false,
		a.pubKey:     false,
	} {
		resp, err := s.ValidateAddress(ctx, &pb.ValidateAddressRequest{Address: address})
		if err != nil || resp.Valid != valid {
			t.Errorf("ValidateAddress %s: got %v, %v, want %v", address, resp,
				err, valid)
		}
	}

	output := []*pb.CreateTransactionRequest_Output{{Address: a.cashAddr, Amount: 1000}}
	creates := []struct {
		name string
		req  *pb.CreateTransactionRequest
	}{
		{"dust output", &pb.CreateTransactionRequest{
			Outputs: []*pb.CreateTransactionRequest_Output{
				{Address: a.cashAddr, Amount: 545}}}},
		{"low fee rate", &pb.CreateTransactionRequest{Outputs: output,
			SatPerKbFee: 500}},
		{"negative confirmations", &pb.CreateTransactionRequest{
			Outputs: output, RequiredConfirmations: -1}},
	}
	for _, test := range creates {
		_, err := s.CreateTransaction(ctx, test.req)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateTransaction with %s: got %v, want InvalidArgument",
				test.name, err)
		}
	}
	// A valid request gets past validation and fails on the missing funds
	// and chain backend instead.
	_, err = s.CreateTransaction(ctx, &pb.CreateTransactionRequest{Outputs: output})
	if err == nil || status.Code(err) == codes.InvalidArgument {
		t.Errorf("valid CreateTransaction: got %v", err)
	}

	_, err = s.SweepAccount(ctx, &pb.SweepAccountRequest{SweepToAddress: a.pubKey})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("SweepAccount to a public key: got %v, want InvalidArgument", err)
	}
	_, err = s.SweepAccount(ctx, &pb.SweepAccountRequest{SweepToAddress: a.cashAddr})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("SweepAccount of an empty account: got %v, want FailedPrecondition", err)
	}

	_, err = s.PostPayment(ctx, &pb.PostPaymentRequest{PaymentUrl: "https://merchant.example/i/1"})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("PostPayment without a refund output: got %v, want InvalidArgument", err)
	}
}
