package bcmr

import (
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/gcash/bchlog"
)

// logged is what the package logs in its tests.
var logged lockedLog

// lockedLog keeps what's written to it, for tests to read while downloads that
// outlive their tests may still write.
type lockedLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// take returns what was logged since it was last called, and forgets it.
func (l *lockedLog) take() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.buf.String()
	l.buf.Reset()
	return s
}

// TestMain has the package log into logged, set once before any test runs, as
// downloads that outlive a test would race with a logger changed later.
func TestMain(m *testing.M) {
	logger := bchlog.NewBackend(&logged).Logger("BCMR")
	logger.SetLevel(bchlog.LevelInfo)
	UseLogger(logger)
	os.Exit(m.Run())
}
