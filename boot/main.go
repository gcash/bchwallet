package boot

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"github.com/gcash/bchd/chaincfg"
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

// grpcStopTimeout is how long the shutdown waits for the gRPC calls that
// stopping the server cancelled to return.  By then the shutdown has released
// the calls that wait on the wallet or the chain backend, so they return right
// away, unless one waits on something else.
const grpcStopTimeout = 10 * time.Second

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
	// Close the log file when the run ends, also when the config fails to
	// load after loadConfig opened the file.
	defer closeLogRotator()

	// Load configuration and parse command line.  This function also
	// initializes logging and configures it accordingly.
	tcfg, _, err := loadConfig(optionalConfigPath)
	if errors.Is(err, errNothingToRun) {
		return nil
	}
	if err != nil {
		return err
	}
	cfg = tcfg

	// Shut down when ctx is done.  This goroutine runs the interrupt
	// handlers added below when this function returns, also on an error,
	// so that nothing started here outlives it.  A shutdown requested while
	// it starts something waits until that has started, so that the
	// shutdown never runs alongside the startup.
	interrupts := newInterruptState()
	defer interrupts.shutdown()
	go func() {
		select {
		case <-ctx.Done():
			interrupts.request()
		case <-interrupts.requested:
		}
	}()

	// A wallet that can't go on syncing with the chain backend stops the
	// run, which then returns its error, instead of panicking.
	var stopped stopError

	// Show version at startup.
	log.Infof("Version %s", version())

	if cfg.Profile != "" {
		profileServer := startProfileServer(cfg.Profile)
		interrupts.addInterruptHandler(func() {
			_ = profileServer.Close()
		})
	}

	if interrupts.stopRequested() {
		return stopped.get()
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
	var chainStoppingOnce sync.Once
	stopReconnecting := func() {
		chainStoppingOnce.Do(func() { close(chainStopping) })
	}
	go func() {
		defer close(chainDone)
		rpcClientConnectLoop(legacyRPCServer, loader, chainStopping,
			chainQuit)
	}()

	// Add interrupt handlers to shutdown the various process components
	// before exiting.  Interrupt handlers run in LIFO order, so the ones
	// added first run last.  The shutdown:
	//
	//  1. stops the chain backend from reconnecting,
	//  2. stops the legacy RPC server,
	//  3. starts stopping the gRPC server, which cancels its calls,
	//  4. unloads the wallet, which stops its chain client,
	//  5. stops the chain backend, and
	//  6. waits for the gRPC calls, which the steps before released from
	//     waiting on the wallet and the chain backend, and unloads a wallet
	//     that one of them loaded meanwhile.
	unloadWallet := func() {
		err := loader.UnloadWallet()
		if err != nil && err != wallet.ErrNotLoaded {
			log.Errorf("Failed to close wallet: %v", err)
		}
	}
	grpcStopped := make(chan struct{})
	var grpcStopOnce sync.Once
	stopGRPC := func() {
		grpcStopOnce.Do(func() {
			log.Warn("Stopping RPC server...")
			go func() {
				rpcs.Stop()
				close(grpcStopped)
			}()
		})
	}
	if rpcs != nil {
		interrupts.addInterruptHandler(func() {
			stopGRPC()
			select {
			case <-grpcStopped:
			case <-time.After(grpcStopTimeout):
				log.Errorf("RPC calls still running %v after the "+
					"RPC server stopped", grpcStopTimeout)
			}
			rpcserver.StopServices(rpcs)
			unloadWallet()
			log.Info("RPC server shutdown")
		})
	}
	interrupts.addInterruptHandler(func() {
		log.Info("Stopping chain backend...")
		stopReconnecting()
		close(chainQuit)
		<-chainDone
		log.Info("Chain backend shutdown")
	})
	interrupts.addInterruptHandler(unloadWallet)
	if rpcs != nil {
		interrupts.addInterruptHandler(stopGRPC)
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
			case <-interrupts.requested:
			}
		}()
	}

	// Run first on shutdown, so that the chain backend does not reconnect
	// when the wallet stops its client.
	interrupts.addInterruptHandler(stopReconnecting)

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

	// Set up every wallet the loader loads, also one that a client opens
	// over gRPC after closing another, and take it out of the RPC servers
	// before it is unloaded.  This runs before the chain backend's callback
	// syncs the wallet, so that the wallet's fatal errors stop the run.
	loader.OnLoad(func(w *wallet.Wallet) {
		w.SetProxyDialer(proxyDialer)
		w.SetFatalErrorHandler(func(err error) {
			stopped.stopWith(interrupts, err)
		})
		if rpcs != nil {
			rpcserver.StartWalletService(rpcs, w)
		}
		if legacyRPCServer != nil {
			legacyRPCServer.RegisterWallet(w)
		}
	})
	loader.OnUnload(func(*wallet.Wallet) {
		if rpcs != nil {
			rpcserver.StopWalletService(rpcs)
		}
		if legacyRPCServer != nil {
			legacyRPCServer.RegisterWallet(nil)
		}
	})

	// Clients can load wallets over gRPC now that the loader sets them up.
	if rpcs != nil {
		rpcserver.StartWalletLoaderService(rpcs, loader, activeNet)
	}

	if !cfg.NoInitialLoad && !interrupts.stopRequested() {
		// Load the wallet database.  It must have been created already
		// or this will return an appropriate error.
		_, err = loader.OpenExistingWallet([]byte(cfg.WalletPass),
			ConsolePrompts)
		if err != nil {
			log.Error(err)
			return err
		}
	}

	<-interrupts.requested
	interrupts.shutdown()
	log.Info("Shutdown complete")
	return stopped.get()
}

// stopError holds the error that stopped a run of the wallet, if any.
type stopError struct {
	mtx sync.Mutex
	err error
}

// stopWith records err as the reason the run stops, unless an error was
// recorded already, and requests the shutdown of the run.  It does not block.
func (s *stopError) stopWith(interrupts *interruptState, err error) {
	log.Errorf("Stopping the wallet: %v", err)
	s.mtx.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mtx.Unlock()
	interrupts.request()
}

// get returns the error recorded by stopWith, or nil.
func (s *stopError) get() error {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.err
}

// startProfileServer serves the profiling handlers on port with a server and
// mux of its own, so that each run of the wallet in a process has its own
// server, which it closes when it stops.  The mux passes /debug/ to
// http.DefaultServeMux, where importing net/http/pprof, as the bchwallet
// command does, registers the profiling handlers.
func startProfileServer(port string) *http.Server {
	listenAddr := net.JoinHostPort("", port)
	mux := http.NewServeMux()
	mux.Handle("/debug/", http.DefaultServeMux)
	mux.Handle("/", http.RedirectHandler("/debug/pprof", http.StatusSeeOther))
	server := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Infof("Profile server listening on %s", listenAddr)
		err := server.ListenAndServe()
		if !errors.Is(err, http.ErrServerClosed) {
			log.Errorf("%v", err)
		}
	}()
	return server
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
// has stopped and its database is closed.  It doesn't wait for a connection
// to a bchd RPC server that is being attempted.  When quit is closed it stops
// the chain client itself, for when no loaded wallet stopped it.
func rpcClientConnectLoop(legacyRPCServer *legacyrpc.Server, loader *wallet.Loader,
	stopping, quit <-chan struct{}) {

	var rpcConfig *chainRPCConfig
	if !cfg.UseSPV {
		rpcConfig = &chainRPCConfig{
			params:     activeNet.Params,
			connect:    cfg.RPCConnect,
			user:       cfg.BchdUsername,
			pass:       cfg.BchdPassword,
			certs:      readCAFile(),
			disableTLS: cfg.DisableClientTLS,
		}
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
			var rpcClient *chain.RPCClient
			rpcClient, err = startChainRPCUnlessStopping(rpcConfig, stopping)
			switch {
			case err != nil:
				err = fmt.Errorf("unable to open connection to "+
					"consensus RPC server: %v", err)
			case rpcClient == nil:
				// The wallet is shutting down.
				return
			default:
				chainClient = rpcClient
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
			// The loader restarts it unless it was unloaded
			// meanwhile, as by the shutdown.
			loader.RestartWallet(loadedWallet)
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

// chainRPCConfig holds the RPC options of a run's config that starting a
// client for a bchd server needs.  They are copied before a start, since a
// connection attempt that the run stops waiting for can outlive it, and must
// then not read the config of another run in the process.
type chainRPCConfig struct {
	params     *chaincfg.Params
	connect    string
	user       string
	pass       string
	certs      []byte
	disableTLS bool
}

// startChainRPC opens a RPC client connection to a bchd server for blockchain
// services.  It makes one connection attempt, so that it returns while the
// server is unreachable, and rpcClientConnectLoop tries again.  A client that
// fails to start is stopped, since it would otherwise reconnect on its own
// after its start connected and then disconnected it, as for another network.
func startChainRPC(c *chainRPCConfig) (*chain.RPCClient, error) {
	log.Infof("Attempting RPC client connection to %v", c.connect)
	rpcc, err := chain.NewRPCClient(c.params, c.connect, c.user, c.pass,
		c.certs, c.disableTLS, 1)
	if err != nil {
		return nil, err
	}
	if err := rpcc.Start(); err != nil {
		rpcc.Stop()
		rpcc.WaitForShutdown()
		return nil, err
	}
	return rpcc, nil
}

// startChainRPCUnlessStopping starts a client like startChainRPC, but returns
// a nil client and no error as soon as stopping is closed, without waiting for
// a connection attempt in progress, whose client is stopped once the attempt
// ends.
func startChainRPCUnlessStopping(c *chainRPCConfig, stopping <-chan struct{}) (
	*chain.RPCClient, error) {

	type started struct {
		client *chain.RPCClient
		err    error
	}
	result := make(chan started, 1)
	go func() {
		client, err := startChainRPC(c)
		result <- started{client, err}
	}()

	select {
	case r := <-result:
		return r.client, r.err
	case <-stopping:
		go func() {
			if r := <-result; r.client != nil {
				r.client.Stop()
				r.client.WaitForShutdown()
			}
		}()
		return nil, nil
	}
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
