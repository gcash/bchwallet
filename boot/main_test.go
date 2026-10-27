package boot

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gcash/bchwallet/internal/cfgutil"
)

// TestStopWith ensures that a fatal wallet error stops the run, and that the
// run keeps the first error that stopped it.
func TestStopWith(t *testing.T) {
	handleSignals := HandleSignals
	HandleSignals = false
	defer func() { HandleSignals = handleSignals }()

	interrupts := newInterruptState()
	defer interrupts.shutdown()

	var stopped stopError
	if err := stopped.get(); err != nil {
		t.Fatalf("got %v before any error", err)
	}

	first := errors.New("unable to synchronize wallet to chain")
	stopped.stopWith(interrupts, first)
	stopped.stopWith(interrupts, errors.New("later error"))

	select {
	case <-interrupts.requested:
	case <-time.After(10 * time.Second):
		t.Fatal("the run was not stopped")
	}
	if err := stopped.get(); !errors.Is(err, first) {
		t.Fatalf("got %v, want %v", err, first)
	}
}

// TestShutdownBeforeHandlers ensures that the handlers added after a shutdown
// was requested run when the run shuts down, in LIFO order, on the run's
// goroutine, and that one added after the shutdown runs right away.
func TestShutdownBeforeHandlers(t *testing.T) {
	handleSignals := HandleSignals
	HandleSignals = false
	defer func() { HandleSignals = handleSignals }()

	interrupts := newInterruptState()
	interrupts.request()
	<-interrupts.requested

	var ran []int
	for i := 1; i <= 3; i++ {
		i := i
		interrupts.addInterruptHandler(func() { ran = append(ran, i) })
	}
	interrupts.shutdown()
	interrupts.addInterruptHandler(func() { ran = append(ran, 4) })
	if fmt.Sprint(ran) != "[3 2 1 4]" {
		t.Fatalf("handlers ran in the order %v, want [3 2 1 4]", ran)
	}
}

// freeAddr returns a loopback address with a port that is free right now.
func freeAddr(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// runConfig writes the config of a regtest wallet, which loads none at
// startup, with extra options, and returns its path and the address of its
// gRPC server.
func runConfig(t *testing.T, extra string) (string, string) {
	t.Helper()

	dir := t.TempDir()
	addr := freeAddr(t)
	conf := fmt.Sprintf("[Application Options]\n\nappdata=%s\nlogdir=%s\n"+
		"experimentalrpclisten=%s\nnoservertls=1\nregtest=1\n"+
		"noinitialload=1\n%s", dir, filepath.Join(dir, "logs"), addr, extra)
	path := filepath.Join(dir, "bchwallet.conf")
	if err := os.WriteFile(path, []byte(conf), 0600); err != nil {
		t.Fatal(err)
	}
	return path, addr
}

// startRun runs the wallet with the config at path until ctx is done, as an
// app that embeds it does, and returns the channel its error comes on.
func startRun(t *testing.T, ctx context.Context, path string) <-chan error {
	t.Helper()

	handleSignals, args := HandleSignals, Args
	HandleSignals, Args = false, []string{}
	t.Cleanup(func() { HandleSignals, Args = handleSignals, args })

	done := make(chan error, 1)
	go func() { done <- WalletMainContext(ctx, &path) }()
	return done
}

// waitRun fails the test unless the run returns without an error within the
// timeout, and checks that it freed its gRPC address.
func waitRun(t *testing.T, done <-chan error, addr string, timeout time.Duration) {
	t.Helper()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the run ended with %v", err)
		}
	case <-time.After(timeout):
		t.Fatalf("the run didn't stop within %v", timeout)
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the run left its gRPC address in use: %v", err)
	}
	_ = l.Close()
}

// waitListening waits until the gRPC server of a run accepts connections.
func waitListening(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the gRPC server doesn't listen: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestWalletMainContextStopEarly ensures that a run stopped before it is done
// starting shuts down and frees what it started.  Its shutdown hung when the
// stop came before the run added the handler that stops the chain backend from
// reconnecting.
func TestWalletMainContextStopEarly(t *testing.T) {
	path, addr := runConfig(t, "usespv=1\nconnect=127.0.0.1:1\n")

	// Stopped before it starts, as it starts, and once it serves.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waitRun(t, startRun(t, ctx, path), addr, 30*time.Second)

	ctx, cancel = context.WithCancel(context.Background())
	done := startRun(t, ctx, path)
	cancel()
	waitRun(t, done, addr, 30*time.Second)

	ctx, cancel = context.WithCancel(context.Background())
	done = startRun(t, ctx, path)
	waitListening(t, addr)
	cancel()
	waitRun(t, done, addr, 30*time.Second)
}

// TestWalletMainContextUnreachableBchd ensures that a run with a bchd RPC chain
// backend stops while bchd can't be reached, which hung its shutdown, since
// the client tried to connect forever.
func TestWalletMainContextUnreachableBchd(t *testing.T) {
	path, addr := runConfig(t, "rpcconnect=127.0.0.1:1\nnoclienttls=1\n")

	ctx, cancel := context.WithCancel(context.Background())
	done := startRun(t, ctx, path)
	waitListening(t, addr)

	// Let the chain backend try to connect.
	time.Sleep(time.Second)
	cancel()
	waitRun(t, done, addr, 15*time.Second)
}

// TestStartRPCServersListenerFailure ensures that when the listeners of the
// legacy RPC server can't be created, nothing is left serving, since no
// interrupt handler would stop it and it would outlive the run.
func TestStartRPCServersListenerFailure(t *testing.T) {
	dir := t.TempDir()
	addr := freeAddr(t)
	cfg = &config{
		ExperimentalRPCListeners: []string{addr},
		DisableServerTLS:         true,
		Username:                 "user",
		Password:                 "pass",
		// Not an IP address, which makeListeners can't listen on.
		LegacyRPCListeners: []string{"localhost:1"},
		RPCCert:            cfgutil.NewExplicitString(filepath.Join(dir, "rpc.cert")),
		RPCKey:             cfgutil.NewExplicitString(filepath.Join(dir, "rpc.key")),
	}

	grpcServer, legacyServer, err := startRPCServers(nil)
	if err == nil || grpcServer != nil || legacyServer != nil {
		t.Fatalf("got %v, %v, %v, want an error and no servers",
			grpcServer, legacyServer, err)
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the gRPC address is still in use: %v", err)
	}
	_ = l.Close()
}
