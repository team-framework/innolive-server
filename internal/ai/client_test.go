package ai

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
	response, err := stream.Process([]byte("frame"), 123)
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
