package rpcserver

import (
	"context"
	"fmt"
	"testing"

	pb "github.com/gcash/bchwallet/rpc/walletrpc"
)

// TestVersion checks that the version string Version returns is its major,
// minor and patch versions.
func TestVersion(t *testing.T) {
	resp, err := (&versionServer{}).Version(context.Background(),
		&pb.VersionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%d.%d.%d", resp.Major, resp.Minor, resp.Patch)
	if resp.VersionString != want {
		t.Errorf("version string %s, want %s", resp.VersionString, want)
	}
}
