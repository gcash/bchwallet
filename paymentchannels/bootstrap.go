package paymentchannels

import (
	"context"
	"errors"
	"fmt"
	"github.com/libp2p/go-libp2p-host"
	"github.com/libp2p/go-libp2p-kad-dht"
	inet "github.com/libp2p/go-libp2p-net"
	"github.com/libp2p/go-libp2p-peerstore"
	"math/rand"
	"sync"
	"time"
)

// ErrNotEnoughBootstrapPeers signals that we do not have enough bootstrap
// peers to bootstrap correctly.
var ErrNotEnoughBootstrapPeers = errors.New("not enough bootstrap peers to bootstrap")

type BootstrapConfig struct {
	// MinPeerThreshold governs whether to bootstrap more connections. If the
	// node has less open connections than this number, it will open connections
	// to the bootstrap nodes. From there, the routing system should be able
	// to use the connections to the bootstrap nodes to connect to even more
	// peers. Routing systems like the libp2p-kad-dht do so in their own Bootstrap
	// process, which issues random queries to find more peers.
	MinPeerThreshold int

	// Period governs the periodic interval at which the node will
	// attempt to bootstrap. The bootstrap process is not very expensive, so
	// this threshold can afford to be small (<=30s).
	Period time.Duration

	// ConnectionTimeout determines how long to wait for a bootstrap
	// connection attempt before cancelling it.
	ConnectionTimeout time.Duration

	// BootstrapPeers is a function that returns a set of bootstrap peers
	// for the bootstrap process to use. This makes it possible for clients
	// to control the peers the process uses at any moment.
	BootstrapPeers func() []peerstore.PeerInfo
}

// DefaultBootstrapConfig specifies default sane parameters for bootstrapping.
var DefaultBootstrapConfig = BootstrapConfig{
	MinPeerThreshold:  2,
	Period:            30 * time.Second,
	ConnectionTimeout: (30 * time.Second) / 3,
}

func bootstrapConfigWithPeers(pis []peerstore.PeerInfo) BootstrapConfig {
	cfg := DefaultBootstrapConfig
	cfg.BootstrapPeers = func() []peerstore.PeerInfo {
		return pis
	}
	return cfg
}

// Bootstrap kicks off the dht bootstrapping. This function will periodically
// check the number of open connections and -- if there are too few -- initiate
// connections to well-known bootstrap peers.
func Bootstrap(routing *dht.IpfsDHT, peerHost host.Host, cfg BootstrapConfig) error {
	// the periodic bootstrap function -- the connection supervisor
	periodic := func() {
		if err := bootstrapRound(context.Background(), peerHost, cfg); err != nil {
			log.Debugf("bootstrap error: %s", err)
		}
	}

	ticker := time.NewTicker(cfg.Period)
	go func() {
		for {
			select {
			case <-ticker.C:
				periodic()
			}
		}
	}()

	// Run it once at startup
	periodic()

	// Bootstrap the DHT. This requires open connections first which is why we start
	// the connection supervisor first.
	if _, err := routing.BootstrapWithConfig(dht.DefaultBootstrapConfig); err != nil {
		return err
	}
	return nil
}

func bootstrapRound(ctx context.Context, host host.Host, cfg BootstrapConfig) error {

	ctx, cancel := context.WithTimeout(ctx, cfg.ConnectionTimeout)
	defer cancel()
	id := host.ID()

	// get bootstrap peers from config. retrieving them here makes
	// sure we remain observant of changes to client configuration.
	peers := cfg.BootstrapPeers()

	// determine how many bootstrap connections to open
	connected := host.Network().Peers()
	if len(connected) >= cfg.MinPeerThreshold {
		log.Debugf("%s core bootstrap skipped -- connected to %d (> %d) nodes",
			id, len(connected), cfg.MinPeerThreshold)
		return nil
	}
	numToDial := cfg.MinPeerThreshold - len(connected)

	// filter out bootstrap nodes we are already connected to
	var notConnected []peerstore.PeerInfo
	for _, p := range peers {
		if host.Network().Connectedness(p.ID) != inet.Connected {
			notConnected = append(notConnected, p)
		}
	}

	// if connected to all bootstrap peer candidates, exit
	if len(notConnected) < 1 {
		log.Debugf("%s no more bootstrap peers to create %d connections", id, numToDial)
		return ErrNotEnoughBootstrapPeers
	}

	// connect to a random susbset of bootstrap candidates
	randSubset := randomSubsetOfPeers(notConnected, numToDial)

	log.Debugf("%s bootstrapping to %d nodes: %s", id, numToDial, randSubset)
	return bootstrapConnect(ctx, host, randSubset)
}

func bootstrapConnect(ctx context.Context, ph host.Host, peers []peerstore.PeerInfo) error {
	if len(peers) < 1 {
		return ErrNotEnoughBootstrapPeers
	}

	errs := make(chan error, len(peers))
	var wg sync.WaitGroup
	for _, p := range peers {

		// performed asynchronously because when performed synchronously, if
		// one `Connect` call hangs, subsequent calls are more likely to
		// fail/abort due to an expiring context.
		// Also, performed asynchronously for dial speed.

		wg.Add(1)
		go func(p peerstore.PeerInfo) {
			defer wg.Done()
			log.Debugf("%s bootstrapping to %s", ph.ID(), p.ID)

			ph.Peerstore().AddAddrs(p.ID, p.Addrs, peerstore.PermanentAddrTTL)
			if err := ph.Connect(ctx, p); err != nil {
				log.Debugf("failed to bootstrap with %v: %s", p.ID, err)
				errs <- err
				return
			}
			log.Infof("bootstrapped with %v", p.ID)
		}(p)
	}
	wg.Wait()

	// our failure condition is when no connection attempt succeeded.
	// So drain the errs channel, counting the results.
	close(errs)
	count := 0
	var err error
	for err = range errs {
		if err != nil {
			count++
		}
	}
	if count == len(peers) {
		return fmt.Errorf("failed to bootstrap. %s", err)
	}
	return nil
}

func randomSubsetOfPeers(in []peerstore.PeerInfo, max int) []peerstore.PeerInfo {
	n := IntMin(max, len(in))
	var out []peerstore.PeerInfo
	for _, val := range rand.Perm(len(in)) {
		out = append(out, in[val])
		if len(out) >= n {
			break
		}
	}
	return out
}

// IntMin returns the smaller of x or y.
func IntMin(x, y int) int {
	if x < y {
		return x
	}
	return y
}
