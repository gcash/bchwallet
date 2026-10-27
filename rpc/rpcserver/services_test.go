package rpcserver

import (
	"context"
	"testing"
	"time"

	pb "github.com/gcash/bchwallet/rpc/walletrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRescanWithoutChainClient checks that Rescan fails right away without a
// chain client, instead of waiting for one until the wallet stops, which held up
// the stop of the gRPC server, as it waits for its handlers.
func TestRescanWithoutChainClient(t *testing.T) {
	s := newNetworkTestServer(t, nil)

	done := make(chan error, 1)
	go func() {
		_, err := s.Rescan(context.Background(), &pb.RescanRequest{})
		done <- err
	}()
	select {
	case err := <-done:
		if status.Code(err) != codes.FailedPrecondition {
			t.Errorf("got %v, want FailedPrecondition", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Rescan waits for a chain client")
	}
}
