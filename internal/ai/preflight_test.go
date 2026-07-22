package ai

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// badStatusServer echoes frames back but reports a non-success status, standing
// in for an AI server that cannot process the sent wire format.
type badStatusServer struct {
	aiv1.UnimplementedAiProcessorServer
}

func (badStatusServer) ProcessVideo(stream grpc.BidiStreamingServer[aiv1.VideoChunk, aiv1.ProcessedVideoChunk]) error {
	for {
		request, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&aiv1.ProcessedVideoChunk{Timestamp: request.Timestamp, StatusMessage: "failed"}); err != nil {
			return err
		}
	}
}

func preflightClient(t *testing.T, impl aiv1.AiProcessorServer) *Client {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	aiv1.RegisterAiProcessorServer(grpcServer, impl)
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)

	connection, err := grpc.DialContext(
		context.Background(),
		"passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{address: "bufnet", conn: connection, client: aiv1.NewAiProcessorClient(connection), timeout: time.Second}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestPreflightPassesAgainstSuccessServer(t *testing.T) {
	client := preflightClient(t, echoAIServer{})
	for _, wireFormat := range []string{"jpeg", "raw"} {
		t.Run(wireFormat, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := client.Preflight(ctx, wireFormat); err != nil {
				t.Fatalf("Preflight(%s) error = %v, want nil", wireFormat, err)
			}
		})
	}
}

func TestPreflightFailsOnNonSuccessStatus(t *testing.T) {
	client := preflightClient(t, badStatusServer{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := client.Preflight(ctx, "jpeg")
	if err == nil {
		t.Fatal("Preflight() error = nil, want a non-success status failure")
	}
	if !strings.Contains(err.Error(), "status") {
		t.Fatalf("Preflight() error = %v, want it to name the bad status", err)
	}
}

func TestPoolPreflightAggregatesTargets(t *testing.T) {
	pool := &Pool{clients: []*Client{preflightClient(t, echoAIServer{}), preflightClient(t, badStatusServer{})}}
	err := pool.Preflight(context.Background(), "jpeg", time.Second)
	if err == nil {
		t.Fatal("Pool.Preflight() error = nil, want the bad target reported")
	}
	if !strings.Contains(err.Error(), "1/2") {
		t.Fatalf("Pool.Preflight() error = %v, want it to report 1/2 targets failed", err)
	}
}
