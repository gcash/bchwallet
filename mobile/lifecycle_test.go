package mobile

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/gcash/bchwallet/rpc/walletrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

const testAuthToken = "4c6966656379636c6554657374546f6b656e"

// writeTestConfig writes a bchwallet.conf for an SPV wallet on regtest whose
// only peer does not exist, so the wallet never syncs but starts and stops
// like it does on a device.
func writeTestConfig(t *testing.T, dir, rpcAddr string, noInitialLoad bool) string {
	t.Helper()

	conf := fmt.Sprintf("[Application Options]\n\nappdata=%s\nlogdir=%s\n"+
		"experimentalrpclisten=%s\nnoservertls=1\nauthtoken=%s\n"+
		"regtest=1\nusespv=1\nconnect=127.0.0.1:1\n",
		dir, filepath.Join(dir, "logs"), rpcAddr, testAuthToken)
	if noInitialLoad {
		conf += "noinitialload=1\n"
	}
	path := filepath.Join(dir, "bchwallet.conf")
	if err := os.WriteFile(path, []byte(conf), 0600); err != nil {
		t.Fatal(err)
	}
	return path
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

// retry calls f until it succeeds or the timeout passes.
func retry(t *testing.T, what string, f func(ctx context.Context) error) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ctx = metadata.AppendToOutgoingContext(ctx, "AuthenticationToken", testAuthToken)
		err := f(ctx)
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v (running: %v, last error: %q)", what, err,
				IsRunning(), LastError())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stopAndCheck stops the wallet and checks that it shut down without an error.
func stopAndCheck(t *testing.T) {
	t.Helper()

	start := time.Now()
	StopWallet()
	if IsRunning() {
		t.Fatalf("wallet still running %v after StopWallet", time.Since(start))
	}
	if msg := LastError(); msg != "" {
		t.Fatalf("wallet run ended with an error: %s", msg)
	}
}

// TestRestartInProcess starts and stops the wallet repeatedly in one process,
// which a mobile app does each time it is closed and opened again.
func TestRestartInProcess(t *testing.T) {
	dir := t.TempDir()
	rpcAddr := freeAddr(t)

	conn, err := grpc.NewClient(rpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	loader := pb.NewWalletLoaderServiceClient(conn)
	walletSvc := pb.NewWalletServiceClient(conn)

	// StopWallet before any StartWallet does nothing.
	StopWallet()

	// The first run creates the wallet over gRPC, as the app does.
	StartWallet(writeTestConfig(t, dir, rpcAddr, true))
	StartWallet(filepath.Join(dir, "bchwallet.conf")) // Already running: no-op.
	var mnemonic string
	retry(t, "GenerateMnemonicSeed", func(ctx context.Context) error {
		resp, err := loader.GenerateMnemonicSeed(ctx,
			&pb.GenerateMnemonicSeedRequest{BitSize: 128})
		if err == nil {
			mnemonic = resp.Mnemonic
		}
		return err
	})
	// Creating the wallet derives its keys, which takes a while with the
	// race detector, so it gets one call with a long timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	ctx = metadata.AppendToOutgoingContext(ctx, "AuthenticationToken", testAuthToken)
	_, err = loader.CreateWallet(ctx, &pb.CreateWalletRequest{
		PrivatePassphrase: []byte("LETMEIN"),
		MnemonicSeed:      mnemonic,
		WalletBirthday:    time.Now().Unix(),
	})
	cancel()
	if err != nil {
		t.Fatalf("CreateWallet: %v", err)
	}
	retry(t, "Ping after CreateWallet", func(ctx context.Context) error {
		_, err := walletSvc.Ping(ctx, &pb.PingRequest{})
		return err
	})
	stopAndCheck(t)

	// Later runs open the existing wallet at startup, reusing the same
	// databases, files and RPC port.
	cfgPath := writeTestConfig(t, dir, rpcAddr, false)
	for i := 0; i < 3; i++ {
		StartWallet(cfgPath)
		retry(t, fmt.Sprintf("run %d: WalletExists", i), func(ctx context.Context) error {
			resp, err := loader.WalletExists(ctx, &pb.WalletExistsRequest{})
			if err == nil && !resp.Exists {
				err = fmt.Errorf("wallet does not exist")
			}
			return err
		})
		retry(t, fmt.Sprintf("run %d: CurrentAddress", i), func(ctx context.Context) error {
			_, err := walletSvc.CurrentAddress(ctx, &pb.CurrentAddressRequest{})
			return err
		})
		stopAndCheck(t)
	}

	// StopWallet when the wallet is not running does nothing.
	StopWallet()
	if IsRunning() {
		t.Fatal("wallet running after StopWallet")
	}

	// Without TLS no key pair is generated, which Android 11 and newer
	// would not allow since it reads the network interface addresses.
	for _, name := range []string{"rpc.cert", "rpc.key"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s was created with noservertls: %v", name, err)
		}
	}
}

// TestStopRightAfterStart starts and stops the wallet at once, before it is
// done starting, which hung its shutdown, and checks that it then still opens
// its wallet, whose database a stop during the startup must not leave open.
func TestStopRightAfterStart(t *testing.T) {
	dir := t.TempDir()
	rpcAddr := freeAddr(t)

	conn, err := grpc.NewClient(rpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	loader := pb.NewWalletLoaderServiceClient(conn)
	walletSvc := pb.NewWalletServiceClient(conn)

	StartWallet(writeTestConfig(t, dir, rpcAddr, true))
	stopAndCheck(t)

	// Create a wallet for the runs below to open as they start.
	StartWallet(writeTestConfig(t, dir, rpcAddr, true))
	var mnemonic string
	retry(t, "GenerateMnemonicSeed", func(ctx context.Context) error {
		resp, err := loader.GenerateMnemonicSeed(ctx,
			&pb.GenerateMnemonicSeedRequest{BitSize: 128})
		if err == nil {
			mnemonic = resp.Mnemonic
		}
		return err
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	ctx = metadata.AppendToOutgoingContext(ctx, "AuthenticationToken", testAuthToken)
	_, err = loader.CreateWallet(ctx, &pb.CreateWalletRequest{
		PrivatePassphrase: []byte("LETMEIN"),
		MnemonicSeed:      mnemonic,
		WalletBirthday:    time.Now().Unix(),
	})
	cancel()
	if err != nil {
		t.Fatalf("CreateWallet: %v", err)
	}
	stopAndCheck(t)

	cfgPath := writeTestConfig(t, dir, rpcAddr, false)
	for i := 0; i < 3; i++ {
		StartWallet(cfgPath)
		stopAndCheck(t)
	}

	StartWallet(cfgPath)
	retry(t, "CurrentAddress after the stops", func(ctx context.Context) error {
		_, err := walletSvc.CurrentAddress(ctx, &pb.CurrentAddressRequest{})
		return err
	})
	stopAndCheck(t)
}
