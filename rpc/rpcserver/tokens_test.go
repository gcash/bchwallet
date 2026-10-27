package rpcserver

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gcash/bchd/chaincfg"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	pb "github.com/gcash/bchwallet/rpc/walletrpc"
	"github.com/gcash/bchwallet/wallet"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// addressChain is a chain backend that watches the addresses the wallet gives
// it, as NextAddress needs.
type addressChain struct {
	fakeChain
}

func (c *addressChain) NotifyReceived([]bchutil.Address) error { return nil }

// TestTokenAwareAddresses checks that CurrentAddress and NextAddress return the
// token-aware form of an address on request, and that ValidateAddress tells the
// forms apart.
func TestTokenAwareAddresses(t *testing.T) {
	s := newNetworkTestServer(t, &addressChain{})
	ctx := context.Background()
	params := s.wallet.ChainParams()

	plain, err := s.CurrentAddress(ctx, &pb.CurrentAddressRequest{})
	if err != nil {
		t.Fatal(err)
	}
	aware, err := s.CurrentAddress(ctx,
		&pb.CurrentAddressRequest{TokenAware: true})
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.NextAddress(ctx, &pb.NextAddressRequest{TokenAware: true})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		address    string
		tokenAware bool
	}{
		{plain.Address, false},
		{aware.Address, true},
		{next.Address, true},
	}
	var hashes [][]byte
	for _, test := range tests {
		addr, err := bchutil.DecodeAddress(test.address, params)
		if err != nil {
			t.Fatalf("%s: %v", test.address, err)
		}
		hashes = append(hashes, addr.ScriptAddress())
		ta, ok := addr.(bchutil.TokenAwareAddress)
		if !ok || ta.IsTokenAware() != test.tokenAware {
			t.Errorf("%s: token-aware %v, want %v", test.address,
				ok && ta.IsTokenAware(), test.tokenAware)
		}

		resp, err := s.ValidateAddress(ctx,
			&pb.ValidateAddressRequest{Address: test.address})
		if err != nil || !resp.Valid || resp.TokenAware != test.tokenAware {
			t.Errorf("ValidateAddress(%s): got %v, %v", test.address,
				resp, err)
		}
	}
	if !bytes.Equal(hashes[0], hashes[1]) {
		t.Error("the token-aware current address has another key")
	}

	resp, err := s.ValidateAddress(ctx,
		&pb.ValidateAddressRequest{Address: "not an address"})
	if err != nil || resp.Valid || resp.TokenAware {
		t.Errorf("ValidateAddress(not an address): got %v, %v", resp, err)
	}
}

// TestMarshalTokens checks that tokens are marshaled with their category in the
// byte order of transaction IDs, as explorers show it, and that an output
// without tokens leaves the field unset.
func TestMarshalTokens(t *testing.T) {
	category := [32]byte{0: 0xaa, 31: 0x01}
	want := "01" + strings.Repeat("00", 30) + "aa"
	token := wire.TokenData{
		CategoryID: category,
		Amount:     5,
		Commitment: []byte{7},
		BitField: wire.HAS_AMOUNT | wire.HAS_NFT |
			wire.HAS_COMMITMENT_LENGTH | wire.MINTING,
	}

	outputs := marshalTransactionOutputs([]wallet.TransactionSummaryOutput{
		{Index: 0, Token: token},
		{Index: 1},
	})
	got := outputs[0].Token
	if got == nil || got.Category != want || got.Amount != 5 || !got.HasNft ||
		got.Capability != pb.TokenData_MINTING ||
		!bytes.Equal(got.Commitment, []byte{7}) {

		t.Errorf("output token: got %v, want category %s", got, want)
	}
	if outputs[1].Token != nil {
		t.Errorf("output without tokens: got %v", outputs[1].Token)
	}

	inputs := marshalTransactionInputs([]wallet.TransactionSummaryInput{
		{PreviousToken: token},
	})
	if inputs[0].PreviousToken == nil ||
		inputs[0].PreviousToken.Category != want {

		t.Errorf("input token: got %v", inputs[0].PreviousToken)
	}

	op := wire.OutPoint{Hash: chainhash.Hash{3}, Index: 2}
	bals := marshalTokenBalances([]wallet.TokenBalance{{
		Category: category,
		Amount:   5,
		NFTs: []wallet.TokenNFT{{
			OutPoint:   op,
			Capability: wire.MUTABLE,
			Commitment: []byte{7},
		}},
	}})
	if len(bals) != 1 || bals[0].Category != want || bals[0].Amount != 5 ||
		len(bals[0].Nfts) != 1 {

		t.Fatalf("balances: got %v", bals)
	}
	nft := bals[0].Nfts[0]
	if !bytes.Equal(nft.TransactionHash, op.Hash[:]) || nft.OutputIndex != 2 ||
		nft.Capability != pb.TokenData_MUTABLE ||
		!bytes.Equal(nft.Commitment, []byte{7}) {

		t.Errorf("NFT: got %v", nft)
	}
}

// TestRequestTokenOutputs checks that requestOutputs reads the tokens an output
// sends, and refuses malformed tokens and tokens to an address that isn't
// token-aware.
func TestRequestTokenOutputs(t *testing.T) {
	a := newTestAddresses(t)
	category := "01" + strings.Repeat("00", 30) + "aa"
	nftHash := chainhash.Hash{3}
	token := func(category string, amount uint64) *pb.CreateTransactionRequest_TokenOutput {
		return &pb.CreateTransactionRequest_TokenOutput{
			Category: category,
			Amount:   amount,
		}
	}

	outputs, nfts, err := requestOutputs([]*pb.CreateTransactionRequest_Output{
		{Address: a.tokenAware, Token: token(category, 5)},
		{Address: a.tokenAware, Amount: 1000,
			Token: &pb.CreateTransactionRequest_TokenOutput{
				Category:           category,
				NftTransactionHash: nftHash[:],
				NftOutputIndex:     2,
			}},
	}, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	want := [32]byte{0: 0xaa, 31: 0x01}
	if len(outputs) != 2 || outputs[0].Value != 0 ||
		outputs[0].TokenData.CategoryID != want ||
		outputs[0].TokenData.Amount != 5 ||
		outputs[0].TokenData.BitField != wire.HAS_AMOUNT || nfts[0] != nil {

		t.Errorf("fungible output: got %+v, NFT %v", outputs[0], nfts[0])
	}
	if outputs[1].Value != 1000 || outputs[1].TokenData.CategoryID != want ||
		!outputs[1].TokenData.IsEmpty() || nfts[1] == nil ||
		*nfts[1] != (wire.OutPoint{Hash: nftHash, Index: 2}) {

		t.Errorf("NFT output: got %+v, NFT %v", outputs[1], nfts[1])
	}

	invalid := []struct {
		name string
		out  *pb.CreateTransactionRequest_Output
	}{
		{"address that isn't token-aware",
			&pb.CreateTransactionRequest_Output{Address: a.cashAddr,
				Token: token(category, 5)}},
		{"legacy address",
			&pb.CreateTransactionRequest_Output{Address: a.legacy,
				Token: token(category, 5)}},
		{"short category",
			&pb.CreateTransactionRequest_Output{Address: a.tokenAware,
				Token: token("abcd", 5)}},
		{"category that isn't hex",
			&pb.CreateTransactionRequest_Output{Address: a.tokenAware,
				Token: token(strings.Repeat("zz", 32), 5)}},
		{"neither an amount nor an NFT",
			&pb.CreateTransactionRequest_Output{Address: a.tokenAware,
				Token: token(category, 0)}},
		{"more tokens than can exist",
			&pb.CreateTransactionRequest_Output{Address: a.tokenAware,
				Token: token(category, wire.MAX_FT_AMOUNT+1)}},
		{"short NFT hash",
			&pb.CreateTransactionRequest_Output{Address: a.tokenAware,
				Token: &pb.CreateTransactionRequest_TokenOutput{
					Category:           category,
					NftTransactionHash: nftHash[:31],
				}}},
		{"negative amount",
			&pb.CreateTransactionRequest_Output{Address: a.tokenAware,
				Amount: -1, Token: token(category, 5)}},
	}
	for _, test := range invalid {
		_, _, err := requestOutputs(
			[]*pb.CreateTransactionRequest_Output{test.out},
			&chaincfg.MainNetParams)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: got %v, want InvalidArgument", test.name, err)
		}
	}
}

// TestTokenErrorCodes checks the gRPC codes of the wallet's errors with
// tokens: a send it refuses is an invalid argument, and a burn it won't sign
// a failed precondition.
func TestTokenErrorCodes(t *testing.T) {
	send := fmt.Errorf("%w: too few tokens", wallet.ErrTokenSend)
	if code := errorCode(send); code != codes.InvalidArgument {
		t.Errorf("token send: got %v, want InvalidArgument", code)
	}
	burn := fmt.Errorf("%w of category 00", wallet.ErrTokenBurn)
	if code := errorCode(burn); code != codes.FailedPrecondition {
		t.Errorf("token burn: got %v, want FailedPrecondition", code)
	}
}
