package mobile

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/gcash/bchwallet/rpc/walletrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// TestRunOnEachNetwork creates an SPV wallet on testnet4, chipnet and mainnet,
// one after the other in this process, and checks that each run uses its own
// network, also when its config selects none after one that did.
func TestRunOnEachNetwork(t *testing.T) {
	runs := []struct {
		option string // Config file line that selects the network.
		dir    string // Directory of the network's wallet.
	}{
		{"testnet4=1", "testnet4"},
		{"chipnet=1", "chipnet"},
		{"", "mainnet"},
	}
	for _, run := range runs {
		appData := t.TempDir()
		rpcAddr := freeAddr(t)
		conf := fmt.Sprintf("[Application Options]\n\nappdata=%s\nlogdir=%s\n"+
			"experimentalrpclisten=%s\nnoservertls=1\nauthtoken=%s\n"+
			"usespv=1\nconnect=127.0.0.1:1\nnoinitialload=1\n%s\n",
			appData, filepath.Join(appData, "logs"), rpcAddr, testAuthToken,
			run.option)
		cfgPath := filepath.Join(appData, "bchwallet.conf")
		if err := os.WriteFile(cfgPath, []byte(conf), 0600); err != nil {
			t.Fatal(err)
		}

		conn, err := grpc.NewClient(rpcAddr,
			grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		loader := pb.NewWalletLoaderServiceClient(conn)

		StartWallet(cfgPath)
		var mnemonic string
		retry(t, run.dir+": GenerateMnemonicSeed", func(ctx context.Context) error {
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
			t.Fatalf("%s: CreateWallet: %v", run.dir, err)
		}
		stopAndCheck(t)
		_ = conn.Close()

		walletDB := filepath.Join(appData, run.dir, "wallet.db")
		if _, err := os.Stat(walletDB); err != nil {
			t.Errorf("%s: no wallet in %s: %v", run.dir, walletDB, err)
		}
	}
}
