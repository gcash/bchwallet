// Copyright (c) 2013-2014 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package boot

import (
	"os"
	"os/signal"
	"sync"
)

// signals defines the signals that are handled to do a clean shutdown.
// Conditional compilation is used to also include SIGTERM on Unix.
var signals = []os.Signal{os.Interrupt}

// HandleSignals controls whether WalletMain shuts the wallet down when the
// process receives one of the signals above.  Programs that embed the wallet,
// such as the mobile package, turn it off so that the host process keeps its
// own signal handling.  It must be set before WalletMain is called.
var HandleSignals = true

// interruptState coordinates the clean shutdown of one run of WalletMain.
// Each run gets its own, so that a process can run the wallet again after it
// shut down.  A signal or a shutdown request only marks the run as stopping:
// the run's goroutine runs the interrupt handlers once it is done starting
// what it was starting, so that the shutdown never runs alongside the startup.
type interruptState struct {
	// signals receives SIGINT (Ctrl+C) and SIGTERM when handleSignals is
	// set.
	signals chan os.Signal

	// handleSignals is whether signals receives the signals, which is
	// HandleSignals when the run started.
	handleSignals bool

	// addHandler is used to add an interrupt handler to the list of
	// handlers to be invoked on shutdown.
	//
	//lint:ignore U1000 kept from when another goroutine ran the handlers.
	addHandler chan func()

	// simulate requests the shutdown from an internal component instead
	// of a signal.
	simulate chan struct{}

	// requested is closed once the shutdown of the run is requested.
	requested chan struct{}

	// handlers are the interrupt handlers, which shutdown runs in LIFO
	// order.  Only the run's goroutine uses them.
	handlers []func()

	// done is closed after all interrupt handlers have run.
	done chan struct{}
}

var (
	interruptMtx sync.Mutex

	// interrupts is the shutdown coordination of the current run of
	// WalletMain, or nil before the first run.
	interrupts *interruptState
)

// newInterruptState creates the shutdown coordination for a new run of
// WalletMain, makes it the current one and starts its handler.
func newInterruptState() *interruptState {
	s := &interruptState{
		signals:       make(chan os.Signal, 1),
		handleSignals: HandleSignals,
		simulate:      make(chan struct{}, 1),
		requested:     make(chan struct{}),
		done:          make(chan struct{}),
	}
	if s.handleSignals {
		signal.Notify(s.signals, signals...)
	}
	go s.mainInterruptHandler()

	interruptMtx.Lock()
	interrupts = s
	interruptMtx.Unlock()
	return s
}

// SimulateInterrupt requests invoking the clean termination process of the
// running wallet by an internal component instead of a SIGINT.
func SimulateInterrupt() {
	interruptMtx.Lock()
	s := interrupts
	interruptMtx.Unlock()
	if s != nil {
		s.request()
	}
}

// request asks for the shutdown of this run.
func (s *interruptState) request() {
	select {
	case s.simulate <- struct{}{}:
	default:
	}
}

// stopRequested returns whether the shutdown of this run was requested.
func (s *interruptState) stopRequested() bool {
	select {
	case <-s.requested:
		return true
	default:
		return false
	}
}

// shutdown requests the shutdown of this run, unless it was requested
// already, and runs its interrupt handlers in LIFO order, unless they ran
// already.  Only the run's goroutine calls it, once it is done starting what
// it was starting.
func (s *interruptState) shutdown() {
	select {
	case <-s.done:
		return
	default:
	}

	s.request()
	<-s.requested
	for i := len(s.handlers) - 1; i >= 0; i-- {
		s.handlers[i]()
	}
	s.handlers = nil
	close(s.done)
}

// mainInterruptHandler waits for SIGINT (Ctrl+C) or a shutdown request, and
// marks the run as stopping, which makes the run's goroutine shut it down.  It
// must be run as a goroutine.
func (s *interruptState) mainInterruptHandler() {
	select {
	case sig := <-s.signals:
		log.Infof("Received signal (%s).  Shutting down...", sig)
	case <-s.simulate:
		log.Info("Received shutdown request.  Shutting down...")
	}

	// Let another signal end the process, should the shutdown hang.
	if s.handleSignals {
		signal.Stop(s.signals)
	}
	close(s.requested)
}

// addInterruptHandler adds a handler to call when a SIGINT (Ctrl+C) is
// received or a shutdown is requested.  Only the run's goroutine calls it.  A
// handler added after the shutdown has already run is called right away.
func (s *interruptState) addInterruptHandler(handler func()) {
	select {
	case <-s.done:
		handler()
	default:
		s.handlers = append(s.handlers, handler)
	}
}
