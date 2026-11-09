package boot

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	chainbcmr "github.com/gcash/bchd/bcmr"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchwallet/bcmr"
	"github.com/gcash/bchwallet/chain"
	"github.com/gcash/bchwallet/wallet"
	"github.com/gcash/neutrino"
	"golang.org/x/net/proxy"
)

// spvSource adapts the neutrino client for the SPV backend.
type spvSource struct {
	*chain.NeutrinoClient
}

// GetBlock asks neutrino, for a read that can give up.
func (s spvSource) GetBlock(hash *chainhash.Hash, quit <-chan struct{}) (*wire.MsgBlock, error) {
	block, err := s.CS.GetBlock(*hash, neutrino.QueryQuit(quit))
	if err != nil {
		return nil, err
	}
	return block.MsgBlock(), nil
}

// SearchOutputs asks neutrino.
func (s spvSource) SearchOutputs(scripts [][]byte, from, to int32, found func(bcmr.OutputFound) bool,
	quit <-chan struct{}) (int32, error) {

	return s.CS.SearchOutputs(scripts, from, to, func(output neutrino.FoundOutput) bool {
		return found(bcmr.OutputFound{Tx: output.Tx, Height: output.Height, BlockHash: output.BlockHash})
	}, nil, quit)
}

// SearchSpends asks neutrino.
func (s spvSource) SearchSpends(outpoints []wire.OutPoint, from, to int32,
	found func(bcmr.SpendFound) []wire.OutPoint, progress func(height int32),
	quit <-chan struct{}) (int32, error) {

	return s.CS.SearchSpends(outpoints, from, to, func(spend neutrino.FoundSpend) []wire.OutPoint {
		return found(bcmr.SpendFound{
			OutPoint:  spend.OutPoint,
			Tx:        spend.SpendingTx,
			Height:    spend.Height,
			BlockHash: spend.BlockHash,
		})
	}, progress, quit)
}

// proxyConnectTimeout bounds connecting through a proxy: to the proxy, its
// handshake, and its connection to the host, which includes the proxy's own name
// lookup, and over Tor a circuit, so it's longer than a direct connection's 10 s.
// A variable for the tests.
var proxyConnectTimeout = 30 * time.Second

// proxiedDial returns the connection for registries when the wallet goes through
// a proxy, or nil for a direct one.
func proxiedDial() (func(ctx context.Context, network, address string) (net.Conn, error), error) {
	if cfg.Proxy == "" {
		return nil, nil
	}
	var auth *proxy.Auth
	if cfg.ProxyUser != "" && cfg.ProxyPass != "" {
		auth = &proxy.Auth{User: cfg.ProxyUser, Password: cfg.ProxyPass}
	}
	dialer, err := proxy.SOCKS5("tcp", cfg.Proxy, auth, proxy.Direct)
	if err != nil {
		return nil, err
	}
	// It keeps to a context, which is what has a connect end in time.
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("the proxy's dialer can't keep to a time")
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if err := bcmr.RefuseLocal(address); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, proxyConnectTimeout)
		defer cancel()
		return contextDialer.DialContext(ctx, network, address)
	}, nil
}

// autoLoad returns what loads by itself: the tokens in the table, with either
// backend.  Any other token waits for the user to ask.  The table holds every
// token up to its height, so this keeps a token minted since from reaching out,
// but one in the table reaches the host it publishes, which sees who asks, and
// someone who runs that host can send it to an address to learn the address's IP.
// Those who mind turn it off with bcmrnoautoload, or use a proxy.
func autoLoad(hints *chainbcmr.Hints) func(chainhash.Hash) bool {
	return func(category chainhash.Hash) bool {
		if hints == nil {
			return false
		}
		_, ok := hints.Lookup(category)
		return ok
	}
}

// startTokenMetadata starts following the wallet's tokens with the chain
// client.  The wallet works without it, so a failure only turns it off.
func startTokenMetadata(w *wallet.Wallet, client chain.Interface) (*bcmr.Manager, error) {
	store, err := bcmr.NewStore(w.Database(),
		filepath.Join(networkDir(cfg.AppDataDir.Value, activeNet.Params), "bcmr-nfts"),
		int64(cfg.BcmrNFTLimit)<<20)
	if err != nil {
		return nil, err
	}
	known := make(map[chainhash.Hash]*bcmr.Metadata)
	if cfg.BcmrKnown != "" {
		data, err := os.ReadFile(cfg.BcmrKnown)
		if err != nil {
			return nil, err
		}
		known, err = bcmr.ParseKnownList(data, time.Now())
		if err != nil {
			return nil, err
		}
	}

	dial, err := proxiedDial()
	if err != nil {
		return nil, err
	}
	config := bcmr.Config{
		Store: store,
		Fetcher: bcmr.NewFetcher(cfg.BcmrGateways,
			filepath.Join(networkDir(cfg.AppDataDir.Value, activeNet.Params), "bcmr-registries"), dial),
		Known: known,
		Ready: client.IsCurrent,
		Tip: func() (*chainhash.Hash, error) {
			hash, _, err := client.GetBestBlock()
			return hash, err
		},
	}
	var hints *chainbcmr.Hints
	if cfg.BcmrHints != "" {
		data, err := os.ReadFile(cfg.BcmrHints)
		if err != nil {
			return nil, err
		}
		hints, err = chainbcmr.ParseHints(data)
		if err != nil {
			return nil, err
		}
	}
	if !cfg.BcmrManual {
		config.AutoLoad = autoLoad(hints)
	}
	switch c := client.(type) {
	case *chain.NeutrinoClient:
		config.Backend = bcmr.NewSPVBackend(spvSource{c}, hints)
		config.FilterRollbacks, config.StopRollbacks = (&neutrino.RescanChainSource{
			ChainService: c.CS,
		}).SubscribeFilterHeaderRollbacks()
	case *chain.RPCClient:
		config.Backend = bcmr.NewFullNodeBackend(c)
	default:
		return nil, fmt.Errorf("no token metadata with %s", client.BackEnd())
	}

	manager, err := bcmr.NewManager(config)
	if err != nil {
		return nil, err
	}
	manager.Start()
	return manager, nil
}

// tokenMetadataHolder is what holds the token metadata a client started.
type tokenMetadataHolder interface {
	SetTokenMetadata(*bcmr.Manager)
}

// tokenMetadataLink ties the token metadata started with a chain client to the
// wallet, until the client stops.
type tokenMetadataLink struct {
	mu      sync.Mutex
	holder  tokenMetadataHolder
	manager *bcmr.Manager
	stopped bool
}

// attach gives the wallet the token metadata started with the client, unless
// the client stopped meanwhile, which stops it instead, so that the wallet never
// holds one that's stopped.
func (l *tokenMetadataLink) attach(holder tokenMetadataHolder, manager *bcmr.Manager) {
	if manager == nil {
		return
	}
	l.mu.Lock()
	stopped := l.stopped
	if !stopped {
		l.holder, l.manager = holder, manager
		holder.SetTokenMetadata(manager)
	}
	l.mu.Unlock()
	if stopped {
		manager.Stop()
	}
}

// detach takes the token metadata from the wallet and stops it, as the client
// stopped, and has any started later stopped too.
func (l *tokenMetadataLink) detach() {
	l.mu.Lock()
	holder, manager := l.holder, l.manager
	l.holder, l.manager, l.stopped = nil, nil, true
	l.mu.Unlock()
	if manager != nil {
		holder.SetTokenMetadata(nil)
		manager.Stop()
	}
}
