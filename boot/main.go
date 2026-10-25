package boot

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"github.com/gcash/bchwallet/chain"
	"github.com/gcash/bchwallet/rpc/legacyrpc"
	"github.com/gcash/bchwallet/rpc/rpcserver"
	"github.com/gcash/bchwallet/wallet"
	"github.com/gcash/bchwallet/walletdb"
	"github.com/gcash/neutrino"
	"github.com/miekg/dns"
)

// chainRetryInterval is how long the chain backend waits before it tries
// again to connect after it failed to start.
const chainRetryInterval = 5 * time.Second

var (
	cfg *config
)

// WalletMain is a work-around main function that is required since deferred
// functions (such as log flushing) are not called with calls to os.Exit.
// Instead, main runs this function and checks for a non-nil error, at which
// point any defers have already run, and if the error is non-nil, the program
// can be exited with an error exit status.
func WalletMain(optionalConfigPath *string) error {
	return WalletMainContext(context.Background(), optionalConfigPath)
}

// WalletMainContext runs the wallet like WalletMain, and also shuts it down
// cleanly once ctx is done.  It returns after the shutdown has completed: the
// RPC servers are stopped and the wallet and chain backend databases are
// closed.  A process that embeds the wallet can therefore call it again to run
// the wallet again.
func WalletMainContext(ctx context.Context, optionalConfigPath *string) error {
	// Load configuration and parse command line.  This function also
	// initializes logging and configures it accordingly.
	tcfg, _, err := loadConfig(optionalConfigPath)
	if err != nil {
		return err
	}
	cfg = tcfg
	defer closeLogRotator()

	// Shut down when ctx is done, and run the interrupt handlers added so
	// far whenever this function returns, also on an error, so that
	// nothing started here outlives it.
	interrupts := newInterruptState()
	defer interrupts.shutdown()
	go func() {
		select {
		case <-ctx.Done():
			interrupts.request()
		case <-interrupts.done:
		}
	}()

	// Show version at startup.
	log.Infof("Version %s", version())

	if cfg.Profile != "" {
		go func() {
			listenAddr := net.JoinHostPort("", cfg.Profile)
			log.Infof("Profile server listening on %s", listenAddr)
			profileRedirect := http.RedirectHandler("/debug/pprof",
				http.StatusSeeOther)
			http.Handle("/", profileRedirect)
			log.Errorf("%v", http.ListenAndServe(listenAddr, nil))
		}()
	}

	dbDir := networkDir(cfg.AppDataDir.Value, activeNet.Params)
	loader := wallet.NewLoader(activeNet.Params, dbDir, true, 250)

	// Create and start HTTP server to serve wallet client connections.
	// This will be updated with the wallet and chain server RPC client
	// created below after each is created.
	rpcs, legacyRPCServer, err := startRPCServers(loader)
	if err != nil {
		log.Errorf("Unable to create RPC servers: %v", err)
		return err
	}

	// Create and start chain RPC client so it's ready to connect to
	// the wallet when loaded later.
	chainStopping := make(chan struct{})
	chainQuit := make(chan struct{})
	chainDone := make(chan struct{})
	go func() {
		defer close(chainDone)
		rpcClientConnectLoop(legacyRPCServer, loader, chainStopping,
			chainQuit)
	}()

	// Add interrupt handlers to shutdown the various process components
	// before exiting.  Interrupt handlers run in LIFO order, so the chain
	// backend (which should be closed last) is added first, followed by the
	// wallet (which uses it), and then the RPC servers.
	interrupts.addInterruptHandler(func() {
		log.Info("Stopping chain backend...")
		close(chainQuit)
		<-chainDone
		log.Info("Chain backend shutdown")
	})
	interrupts.addInterruptHandler(func() {
		err := loader.UnloadWallet()
		if err != nil && err != wallet.ErrNotLoaded {
			log.Errorf("Failed to close wallet: %v", err)
		}
	})
	if rpcs != nil {
		interrupts.addInterruptHandler(func() {
			log.Warn("Stopping RPC server...")
			rpcs.Stop()
			rpcserver.StopServices()
			log.Info("RPC server shutdown")
		})
	}
	if legacyRPCServer != nil {
		interrupts.addInterruptHandler(func() {
			log.Warn("Stopping legacy RPC server...")
			legacyRPCServer.Stop()
			log.Info("Legacy RPC server shutdown")
		})
		go func() {
			select {
			case <-legacyRPCServer.RequestProcessShutdown():
				interrupts.request()
			case <-interrupts.done:
			}
		}()
	}

	// Run first on shutdown, so that the chain backend does not reconnect
	// when the wallet stops its client.
	interrupts.addInterruptHandler(func() {
		close(chainStopping)
	})

	var proxyDialer proxy.Dialer
	if cfg.Proxy != "" {
		var auth *proxy.Auth
		if cfg.ProxyUser != "" && cfg.ProxyPass != "" {
			auth = &proxy.Auth{
				User:     cfg.ProxyUser,
				Password: cfg.ProxyPass,
			}
		}
		proxyDialer, err = proxy.SOCKS5("tcp", cfg.Proxy, auth, proxy.Direct)
		if err != nil {
			log.Errorf("Unable to build the proxy dialer: %v", err)
		}
	}

	loader.RunAfterLoad(func(w *wallet.Wallet) {
		if rpcs != nil {
			rpcserver.StartWalletService(rpcs, w)
		}

		if legacyRPCServer != nil {
			legacyRPCServer.RegisterWallet(w)
		}

		w.SetProxyDialer(proxyDialer)
	})

	if !cfg.NoInitialLoad {
		// Load the wallet database.  It must have been created already
		// or this will return an appropriate error.
		_, err = loader.OpenExistingWallet([]byte(cfg.WalletPass), true)
		if err != nil {
			log.Error(err)
			return err
		}
	}

	<-interrupts.done
	log.Info("Shutdown complete")
	return nil
}

// rpcClientConnectLoop continuously attempts a connection to the consensus RPC
// server.  When a connection is established, the client is used to sync the
// loaded wallet, either immediately or when loaded at a later time.
//
// The legacy RPC is optional.  If set, the connected RPC client will be
// associated with the server for RPC passthrough and to enable additional
// methods.
//
// Once stopping is closed the loop no longer reconnects, and it returns after
// the chain client has shut down and, in SPV mode, the neutrino chain service
// has stopped and its database is closed.  When quit is closed it stops the
// chain client itself, for when no loaded wallet stopped it.
func rpcClientConnectLoop(legacyRPCServer *legacyrpc.Server, loader *wallet.Loader,
	stopping, quit <-chan struct{}) {

	var certs []byte
	if !cfg.UseSPV {
		certs = readCAFile()
	}

	for {
		var (
			chainClient chain.Interface
			stopBackend func()
			err         error
		)

		if cfg.UseSPV {
			chainClient, stopBackend, err = startNeutrino()
		} else {
			chainClient, err = startChainRPC(certs)
			if err != nil {
				err = fmt.Errorf("unable to open connection to "+
					"consensus RPC server: %v", err)
			}
		}
		if err != nil {
			log.Error(err)

			// Wait before trying again, unless the wallet is
			// shutting down.
			select {
			case <-stopping:
				return
			case <-time.After(chainRetryInterval):
			}
			continue
		}

		// Rather than inlining this logic directly into the loader
		// callback, a function variable is used to avoid running any of
		// this after the client disconnects by setting it to nil.  This
		// prevents the callback from associating a wallet loaded at a
		// later time with a client that has already disconnected.  A
		// mutex is used to make this concurrent safe.
		associateRPCClient := func(w *wallet.Wallet) {
			w.SynchronizeRPC(chainClient)
			if legacyRPCServer != nil {
				legacyRPCServer.SetChainServer(chainClient)
			}
		}
		mu := new(sync.Mutex)
		loader.RunAfterLoad(func(w *wallet.Wallet) {
			mu.Lock()
			associate := associateRPCClient
			mu.Unlock()
			if associate != nil {
				associate(w)
			}
		})

		// Stop the client when the wallet shuts down.  The wallet stops
		// it itself when it is loaded, but it may never have been.
		clientDone := make(chan struct{})
		go func() {
			select {
			case <-quit:
				chainClient.Stop()
			case <-clientDone:
			}
		}()

		chainClient.WaitForShutdown()
		close(clientDone)
		if stopBackend != nil {
			stopBackend()
		}

		mu.Lock()
		associateRPCClient = nil
		mu.Unlock()

		select {
		case <-stopping:
			return
		default:
		}

		loadedWallet, ok := loader.LoadedWallet()
		if ok {
			// Do not attempt a reconnect when the wallet was
			// explicitly stopped.
			if loadedWallet.ShuttingDown() {
				return
			}

			loadedWallet.SetChainSynced(false)

			// TODO: Rework the wallet so changing the RPC client
			// does not require stopping and restarting everything.
			loadedWallet.Stop()
			loadedWallet.WaitForShutdown()
			loadedWallet.Start()
		}
	}
}

// startNeutrino opens the neutrino database and starts a neutrino chain
// service with a client for it.  The returned function stops the chain service
// and closes its database, and must be called once the client has shut down.
func startNeutrino() (*chain.NeutrinoClient, func(), error) {
	netDir := networkDir(cfg.AppDataDir.Value, activeNet.Params)
	spvdb, err := walletdb.Create("bdb",
		filepath.Join(netDir, "neutrino.db"), true)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create Neutrino DB: %v", err)
	}
	chainService, err := neutrino.NewChainService(
		neutrino.Config{
			DataDir:       netDir,
			Database:      spvdb,
			ChainParams:   *activeNet.Params,
			ConnectPeers:  cfg.ConnectPeers,
			AddPeers:      cfg.AddPeers,
			BlocksOnly:    cfg.BlocksOnly,
			PersistToDisk: cfg.PersistFilters,
			Proxy:         cfg.Proxy,
			NameResolver:  buildNeutrinoDNSResolver(),
		})
	if err != nil {
		_ = spvdb.Close()
		return nil, nil, fmt.Errorf("couldn't create Neutrino "+
			"ChainService: %v", err)
	}

	stop := func() {
		if err := chainService.Stop(); err != nil {
			log.Errorf("Unable to stop Neutrino ChainService: %v", err)
		}
		if err := spvdb.Close(); err != nil {
			log.Errorf("Unable to close Neutrino DB: %v", err)
		}
	}

	chainClient := chain.NewNeutrinoClient(activeNet.Params, chainService)
	if err := chainClient.Start(); err != nil {
		stop()
		return nil, nil, fmt.Errorf("couldn't start Neutrino client: %v", err)
	}
	return chainClient, stop, nil
}

func readCAFile() []byte {
	// Read certificate file if TLS is not disabled.
	var certs []byte
	if !cfg.DisableClientTLS {
		var err error
		certs, err = os.ReadFile(cfg.CAFile.Value)
		if err != nil {
			log.Warnf("Cannot open CA file: %v", err)
			// If there's an error reading the CA file, continue
			// with nil certs and without the client connection.
			certs = nil
		}
	} else {
		log.Info("Chain server RPC TLS is disabled")
	}

	return certs
}

// startChainRPC opens a RPC client connection to a bchd server for blockchain
// services.  This function uses the RPC options from the global config and
// there is no recovery in case the server is not available or if there is an
// authentication error.  Instead, all requests to the client will simply error.
func startChainRPC(certs []byte) (*chain.RPCClient, error) {
	log.Infof("Attempting RPC client connection to %v", cfg.RPCConnect)
	rpcc, err := chain.NewRPCClient(activeNet.Params, cfg.RPCConnect,
		cfg.BchdUsername, cfg.BchdPassword, certs, cfg.DisableClientTLS, 0)
	if err != nil {
		return nil, err
	}
	err = rpcc.Start()
	return rpcc, err
}

// buildNeutrinoDNSResolver returns a custom DNS resolver designed for use with
// neutrino. Because neutrino is primarily going to run on mobile devices we
// want it to be able to take advantage of orbot's DNS proxy which runs on port
// 5400 on localhost if available. Therefore this resolve function first tries
// localhost and if that fails proceeds to perform a normal DNS lookup. If
// localhost is unavailable it should fail immediately.
func buildNeutrinoDNSResolver() func(host string) ([]net.IP, error) {
	return func(host string) ([]net.IP, error) {
		c := dns.Client{}
		m := dns.Msg{}
		m.SetQuestion(host+".", dns.TypeA)
		r, _, err := c.Exchange(&m, "127.0.0.1:5400")
		if err != nil {
			return net.LookupIP(host)
		}
		var ips []net.IP
		for _, ans := range r.Answer {
			if Arecord, ok := ans.(*dns.A); ok {
				ips = append(ips, Arecord.A)
			}
		}
		return ips, nil
	}
}
