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
// shut down.
type interruptState struct {
	// signals receives SIGINT (Ctrl+C) and SIGTERM when HandleSignals is
	// set.
	signals chan os.Signal

	// addHandler is used to add an interrupt handler to the list of
	// handlers to be invoked on shutdown.
	addHandler chan func()

	// simulate requests the shutdown from an internal component instead
	// of a signal.
	simulate chan struct{}

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
		signals:    make(chan os.Signal, 1),
		addHandler: make(chan func()),
		simulate:   make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	handleSignals := HandleSignals
	if handleSignals {
		signal.Notify(s.signals, signals...)
	}
	go s.mainInterruptHandler(handleSignals)

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

// shutdown requests the shutdown of this run, unless it already happened, and
// waits for its interrupt handlers to finish.
func (s *interruptState) shutdown() {
	s.request()
	<-s.done
}

// mainInterruptHandler listens for SIGINT (Ctrl+C) signals and shutdown
// requests and invokes the registered interruptCallbacks accordingly.  It also
// listens for callback registration.  It must be run as a goroutine.
func (s *interruptState) mainInterruptHandler(handleSignals bool) {
	// interruptCallbacks is a list of callbacks to invoke when a
	// SIGINT (Ctrl+C) is received.
	var interruptCallbacks []func()
	invokeCallbacks := func() {
		if handleSignals {
			signal.Stop(s.signals)
		}

		// run handlers in LIFO order.
		for i := range interruptCallbacks {
			idx := len(interruptCallbacks) - 1 - i
			interruptCallbacks[idx]()
		}
		close(s.done)
	}

	for {
		select {
		case sig := <-s.signals:
			log.Infof("Received signal (%s).  Shutting down...", sig)
			invokeCallbacks()
			return
		case <-s.simulate:
			log.Info("Received shutdown request.  Shutting down...")
			invokeCallbacks()
			return

		case handler := <-s.addHandler:
			interruptCallbacks = append(interruptCallbacks, handler)
		}
	}
}

// addInterruptHandler adds a handler to call when a SIGINT (Ctrl+C) is
// received or a shutdown is requested.  A handler added after the shutdown
// has already run is called right away.
func (s *interruptState) addInterruptHandler(handler func()) {
	select {
	case s.addHandler <- handler:
	case <-s.done:
		handler()
	}
}
