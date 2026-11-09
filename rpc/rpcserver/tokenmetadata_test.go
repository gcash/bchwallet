package rpcserver

import (
	"testing"

	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchwallet/bcmr"
	pb "github.com/gcash/bchwallet/rpc/walletrpc"
)

// TestMarshalTokenInfos ensures the fields of a token reach the API, its NFT
// types are sorted, and a token with nothing to show says so.
func TestMarshalTokenInfos(t *testing.T) {
	tokens := marshalTokenInfos([]bcmr.TokenInfo{
		{
			Category: chainhash.Hash{1},
			Status:   bcmr.StatusVerified,
			Known:    true,
			InHints:  true,
			Burned:   true,
			Metadata: &bcmr.Metadata{
				Name: "Name", Description: "About", Symbol: "SYM", Decimals: 8,
				IconURI: "ipfs://icon", WebURI: "https://web", NFTTypeCount: 2,
			},
		},
		{Category: chainhash.Hash{2}, LookAlike: true, Status: bcmr.StatusFailed},
	}, func(category chainhash.Hash) map[string]bcmr.NFTType {
		if category != (chainhash.Hash{1}) {
			return nil
		}
		return map[string]bcmr.NFTType{
			"02": {Name: "Two"},
			"01": {Name: "One", Description: "First", IconURI: "ipfs://one"},
		}
	})
	if len(tokens) != 2 {
		t.Fatalf("%d tokens", len(tokens))
	}
	token := tokens[0]
	if !token.HasMetadata || token.Category != (chainhash.Hash{1}).String() ||
		token.Name != "Name" || token.Description != "About" || token.Symbol != "SYM" ||
		token.Decimals != 8 || token.IconUri != "ipfs://icon" || token.WebUri != "https://web" ||
		token.Status != pb.TokenMetadataResponse_VERIFIED || !token.Known ||
		!token.LoadsByItself || !token.Burned || token.LookAlike {

		t.Fatalf("got %+v", token)
	}
	if len(token.NftTypes) != 2 || token.NftTypes[0].Commitment != "01" ||
		token.NftTypes[0].Description != "First" || token.NftTypes[0].IconUri != "ipfs://one" ||
		token.NftTypes[1].Name != "Two" {

		t.Fatalf("NFT types %+v", token.NftTypes)
	}
	if tokens[1].HasMetadata || !tokens[1].LookAlike ||
		tokens[1].Status != pb.TokenMetadataResponse_FAILED {

		t.Fatalf("got %+v", tokens[1])
	}
}

// TestStatusNumbers ensures the package's statuses are the API's, which the
// conversion counts on.
func TestStatusNumbers(t *testing.T) {
	for status, want := range map[bcmr.Status]pb.TokenMetadataResponse_Status{
		bcmr.StatusSearching:   pb.TokenMetadataResponse_SEARCHING,
		bcmr.StatusVerified:    pb.TokenMetadataResponse_VERIFIED,
		bcmr.StatusUnavailable: pb.TokenMetadataResponse_UNAVAILABLE,
		bcmr.StatusFailed:      pb.TokenMetadataResponse_FAILED,
	} {
		if pb.TokenMetadataResponse_Status(status) != want {
			t.Errorf("status %d is %v in the API", status, want)
		}
	}
}

// TestMarshalLeavesOutNFTTypes ensures the types only come when asked for, as a
// list of them can be large.
func TestMarshalLeavesOutNFTTypes(t *testing.T) {
	tokens := marshalTokenInfos([]bcmr.TokenInfo{{
		Stale:    true,
		Metadata: &bcmr.Metadata{Name: "N", NFTTypeCount: 1},
	}}, nil)
	if len(tokens[0].NftTypes) != 0 || tokens[0].Name != "N" || !tokens[0].PublicationStale {
		t.Fatalf("got %+v", tokens[0])
	}
}
