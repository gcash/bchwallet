package mobile

import (
	"os"
	"time"

	"github.com/dcrlabs/bchwallet/boot"
)

// StartWallet is the function exposed to the mobile device to start the bchwallet.
// configPath is the path to the bchwallet.conf file that should be saved on your mobile device.
//
// Make sure you save in the config file the correct path on the device to use for `appdata`.
// You will likely also want to the `noinitalload` option to prevent the wallet from blocking
// startup as it waits for input from stdin.
//
// Once the wallet is started you will want to control it using the gRPC API. A `CreateWallet` RPC
// is available which you will need to call first.
func StartWallet(configPath string) {
	go boot.WalletMain(&configPath)
}

// StopWallet will stop the wallet and perform a clean shutdown.
func StopWallet() {
	boot.SimulateInterrupt()
	time.AfterFunc(time.Second*3, func() {
		os.Exit(1)
	})
}
