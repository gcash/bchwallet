package boot

import (
	"context"
	"net"
	"testing"
	"time"

	chainbcmr "github.com/gcash/bchd/bcmr"
	"github.com/gcash/bchd/chaincfg/chainhash"
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
