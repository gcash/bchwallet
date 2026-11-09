package boot

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	chainbcmr "github.com/gcash/bchd/bcmr"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchwallet/bcmr"
	"github.com/gcash/bchwallet/walletdb"
	_ "github.com/gcash/bchwallet/walletdb/bdb"
)

// TestAutoLoad ensures only a token in the table loads by itself, and that with
// no table none does.
func TestAutoLoad(t *testing.T) {
	inTable, notInTable := chainhash.Hash{1}, chainhash.Hash{2}
	hints, err := chainbcmr.ParseHints(chainbcmr.EncodeHints(10, []chainbcmr.Hint{{Category: inTable}}))
	if err != nil {
		t.Fatal(err)
	}
	load := autoLoad(hints)
	if !load(inTable) || load(notInTable) {
		t.Fatal("wrong answer from the table")
	}
	if autoLoad(nil)(inTable) {
		t.Fatal("a token loaded by itself with no table")
	}
}

// TestProxiedDialTimesOut ensures a connection through a proxy that takes it and
// never answers ends at the proxy's time to connect.
func TestProxiedDialTimesOut(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	defer func(saved *config, timeout time.Duration) {
		cfg, proxyConnectTimeout = saved, timeout
	}(cfg, proxyConnectTimeout)
	cfg = &config{Proxy: listener.Addr().String()}
	proxyConnectTimeout = 200 * time.Millisecond

	dial, err := proxiedDial()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		conn, err := dial(context.Background(), "tcp", "example.com:443")
		if conn != nil {
			conn.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("connected through a proxy that never answered")
		}
	case <-time.After(time.Minute):
		t.Fatal("a proxy that never answered held the connection")
	}
}

// fakeHolder records the token metadata it's given.
type fakeHolder struct {
	given []*bcmr.Manager
}

func (h *fakeHolder) SetTokenMetadata(manager *bcmr.Manager) {
	h.given = append(h.given, manager)
}

// TestTokenMetadataLink ensures the wallet holds the token metadata only while
// its client runs, and that one started as the client stopped is stopped, and
// never held.
func TestTokenMetadataLink(t *testing.T) {
	db, err := walletdb.Create("bdb", filepath.Join(t.TempDir(), "wallet.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := bcmr.NewStore(db, t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	stops := 0
	manager, err := bcmr.NewManager(bcmr.Config{Store: store, StopRollbacks: func() { stops++ }})
	if err != nil {
		t.Fatal(err)
	}

	// Started while the client runs.
	link, holder := new(tokenMetadataLink), new(fakeHolder)
	link.attach(holder, manager)
	link.detach()
	if len(holder.given) != 2 || holder.given[0] != manager || holder.given[1] != nil || stops != 1 {
		t.Fatalf("given %v, %d stops", holder.given, stops)
	}

	// Started as the client stopped.
	link, holder = new(tokenMetadataLink), new(fakeHolder)
	link.detach()
	link.attach(holder, manager)
	if len(holder.given) != 0 || stops != 2 {
		t.Fatalf("given %v, %d stops", holder.given, stops)
	}

	// None started.
	link, holder = new(tokenMetadataLink), new(fakeHolder)
	link.attach(holder, nil)
	link.detach()
	if len(holder.given) != 0 {
		t.Fatalf("given %v", holder.given)
	}
}
