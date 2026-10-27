package rpcserver

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	pb "github.com/gcash/bchwallet/rpc/walletrpc"
	"github.com/gcash/bchwallet/wallet"
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
