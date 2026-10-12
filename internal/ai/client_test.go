package ai

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type echoAIServer struct {
	aiv1.UnimplementedAiProcessorServer
}

func (echoAIServer) ProcessVideo(stream grpc.BidiStreamingServer[aiv1.VideoChunk, aiv1.ProcessedVideoChunk]) error {
	for {
		request, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&aiv1.ProcessedVideoChunk{Data: request.Data, Timestamp: request.Timestamp, StatusMessage: "success"}); err != nil {
			return err
		}
	}
}

func (echoAIServer) AddWhitelist(context.Context, *aiv1.FaceData) (*aiv1.WhitelistResponse, error) {
	return &aiv1.WhitelistResponse{StatusMessage: "success", Timestamp: 42}, nil
}

func TestClientUsesStreamingAndWhitelistContracts(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	aiv1.RegisterAiProcessorServer(grpcServer, echoAIServer{})
	go grpcServer.Serve(listener)
	defer grpcServer.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, err := grpc.DialContext(
		ctx,
		"passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{address: "bufnet", conn: connection, client: aiv1.NewAiProcessorClient(connection), timeout: time.Second}
	defer client.Close()

	stream := client.NewStream(ctx, "")
	defer stream.Close()
	response, err := stream.Process([]byte("frame"), 123, 0, 0, "")
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if string(response.GetData()) != "frame" || response.GetTimestamp() != 123 {
		t.Fatalf("unexpected ProcessVideo response: %+v", response)
	}
	whitelist, err := client.AddWhitelist(ctx, "", []byte("face"))
	if err != nil {
		t.Fatalf("AddWhitelist() error = %v", err)
	}
	if whitelist.GetStatusMessage() != "success" {
		t.Fatalf("whitelist status = %q", whitelist.GetStatusMessage())
	}
}

func newStreamTestClient(t *testing.T, server aiv1.AiProcessorServer) *Client {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	aiv1.RegisterAiProcessorServer(grpcServer, server)
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)
	connection, err := grpc.NewClient(
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

// 파이프라인을 내리며 세션 ctx를 취소하면 gRPC는 status Canceled를 돌려준다.
// 호출부가 AI 실패와 구분하도록 context.Canceled로 감싸야 한다(#297).
func TestStreamProcessReportsContextCanceledWhenSessionCanceled(t *testing.T) {
	client := newStreamTestClient(t, echoAIServer{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stream := client.NewStream(ctx, "")
	defer stream.Close()
	_, err := stream.Process([]byte("frame"), 1, 0, 0, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Process() error = %v, want context.Canceled", err)
	}
}

type unavailableAIServer struct {
	aiv1.UnimplementedAiProcessorServer
}

func (unavailableAIServer) ProcessVideo(grpc.BidiStreamingServer[aiv1.VideoChunk, aiv1.ProcessedVideoChunk]) error {
	return status.Error(codes.Unavailable, "worker down")
}

func TestStreamProcessKeepsAIFailureWhenSessionAlive(t *testing.T) {
	client := newStreamTestClient(t, unavailableAIServer{})
	stream := client.NewStream(context.Background(), "")
	defer stream.Close()
	_, err := stream.Process([]byte("frame"), 1, 0, 0, "")
	if err == nil {
		t.Fatal("Process() error = nil, want AI failure")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("Process() error = %v, AI failure must not look like cancellation", err)
	}
}
