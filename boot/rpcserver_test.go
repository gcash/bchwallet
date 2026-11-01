package boot

import (
	"context"
	pb "github.com/gcash/bchwallet/rpc/walletrpc"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestValidateAuthenticationToken(t *testing.T) {
	ctx := context.Background()
	cfg = &config{}
	cfg.AuthToken = "LetMeIn"

	if err := validateAuthenticationToken(ctx); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Failed to refuse an empty context: %v", err)
	}

	md := metadata.Pairs(AuthenticationTokenKey, "LetMeOut")
	wrongCtx := metadata.NewIncomingContext(ctx, md)
	if err := validateAuthenticationToken(wrongCtx); status.Code(err) != codes.Unauthenticated {
		t.Errorf("Failed to refuse a wrong token: %v", err)
	}

	md = metadata.Pairs(AuthenticationTokenKey, cfg.AuthToken)
	ctx = metadata.NewIncomingContext(ctx, md)
	if err := validateAuthenticationToken(ctx); err != nil {
		t.Error("Failed to correctly authenticate context")
	}

	cfg.AuthToken = ""
	if err := validateAuthenticationToken(context.Background()); err != nil {
		t.Errorf("Failed to accept a call without a configured token: %v", err)
	}
}

// testServerStream is a grpc.ServerStream that only has a context.
type testServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *testServerStream) Context() context.Context {
	return s.ctx
}

// TestInterceptorsRecoverPanics ensures a handler that panics makes its call
// fail with Internal instead of crashing the process.
func TestInterceptorsRecoverPanics(t *testing.T) {
	cfg = &config{}
	ctx := context.Background()
	const method = "/walletrpc.VersionService/Version"

	resp, err := interceptUnary(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method},
		func(context.Context, interface{}) (interface{}, error) {
			panic("unary handler bug")
		})
	if resp != nil || status.Code(err) != codes.Internal {
		t.Errorf("unary: got %v, %v, want an Internal error", resp, err)
	}

	err = interceptStreaming(nil, &testServerStream{ctx: ctx},
		&grpc.StreamServerInfo{FullMethod: method},
		func(interface{}, grpc.ServerStream) error {
			panic("streaming handler bug")
		})
	if status.Code(err) != codes.Internal {
		t.Errorf("streaming: got %v, want an Internal error", err)
	}

	resp, err = interceptUnary(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method},
		func(context.Context, interface{}) (interface{}, error) {
			return "ok", nil
		})
	if resp != "ok" || err != nil {
		t.Errorf("unary without a panic: got %v, %v", resp, err)
	}
}

// frameCounter is a client's connection that also reads the frames the server
// sends it, counting all of them and the pings among them, which the server
// starts, unlike the acknowledgements of the client's own.
type frameCounter struct {
	net.Conn
	pw *io.PipeWriter

	mu     sync.Mutex
	frames int
	pings  int
	err    error
}

func countFrames(conn net.Conn) *frameCounter {
	pr, pw := io.Pipe()
	c := &frameCounter{Conn: conn, pw: pw}
	go func() {
		framer := http2.NewFramer(io.Discard, pr)
		for {
			f, err := framer.ReadFrame()
			c.mu.Lock()
			if err != nil {
				c.err = err
				c.mu.Unlock()
				_ = pr.CloseWithError(err)
				return
			}
			c.frames++
			if ping, ok := f.(*http2.PingFrame); ok && !ping.IsAck() {
				c.pings++
			}
			c.mu.Unlock()
		}
	}()
	return c
}

func (c *frameCounter) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		_, _ = c.pw.Write(b[:n])
	}
	return n, err
}

func (c *frameCounter) Close() error {
	_ = c.pw.Close()
	return c.Conn.Close()
}

func (c *frameCounter) counts() (frames, pings int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.frames, c.pings, c.err
}

// TestRPCServerSendsNoPings ensures that the gRPC server doesn't ping its
// clients after their requests.  grpc-go tuned its flow-control windows with
// a ping after each request on a connection that carries small messages, and
// grpc-java's OkHttp transport, which Android apps use, closed the connection
// after answering 10,000 of them, ending the client's notification streams.
func TestRPCServerSendsNoPings(t *testing.T) {
	addr := freeAddr(t)
	cfg = &config{
		ExperimentalRPCListeners: []string{addr},
		DisableServerTLS:         true,
	}
	server, legacyServer, err := startRPCServers(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	if legacyServer != nil {
		t.Fatal("started a legacy RPC server without credentials")
	}

	var counter *frameCounter
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
			if err != nil {
				return nil, err
			}
			counter = countFrames(c)
			return counter, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	const calls = 100
	client := pb.NewVersionServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := 0; i < calls; i++ {
		if _, err := client.Version(ctx, &pb.VersionRequest{}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	// Each call's response has at least two frames, its headers and its
	// trailers, which the counter must have read by the time it returned.
	frames, pings, err := counter.counts()
	if err != nil || frames < 2*calls {
		t.Fatalf("read %d frames of the server's (%v), want at least %d",
			frames, err, 2*calls)
	}
	if pings != 0 {
		t.Errorf("the server sent %d pings during %d calls, want none",
			pings, calls)
	}
}
