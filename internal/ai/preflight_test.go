package ai

import (
	"bytes"
	"context"
	"image/jpeg"
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

// badStatusServer는 프레임을 되돌려주되 성공이 아닌 status를 보고한다. 보낸
// 와이어 포맷을 처리하지 못하는 AI 서버를 대신한다.
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Preflight(ctx, "jpeg", 0); err != nil {
		t.Fatalf("Preflight(jpeg) error = %v, want nil", err)
	}
}

// TestPreflightRejectsRawWireFormat: 이 AI 서버의 proto에는 width/height/pix_fmt
// 필드가 없어서 raw yuv420p는 전달 자체가 불가능하다 — Preflight는 raw 라벨을
// 달고 jpeg 데이터를 조용히 보내는 대신 시끄럽게 거부해야 한다.
func TestPreflightRejectsRawWireFormat(t *testing.T) {
	client := preflightClient(t, echoAIServer{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := client.Preflight(ctx, "raw", 0)
	if err == nil {
		t.Fatal("Preflight(raw) error = nil, want a not-supported error")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("Preflight(raw) error = %v, want it to say raw is not supported", err)
	}
}

func TestPreflightFailsOnNonSuccessStatus(t *testing.T) {
	client := preflightClient(t, badStatusServer{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := client.Preflight(ctx, "jpeg", 0)
	if err == nil {
		t.Fatal("Preflight() error = nil, want a non-success status failure")
	}
	if !strings.Contains(err.Error(), "status") {
		t.Fatalf("Preflight() error = %v, want it to name the bad status", err)
	}
}

func TestPoolPreflightAggregatesTargets(t *testing.T) {
	pool := &Pool{clients: []*Client{preflightClient(t, echoAIServer{}), preflightClient(t, badStatusServer{})}}
	err := pool.Preflight(context.Background(), "jpeg", time.Second, 0)
	if err == nil {
		t.Fatal("Pool.Preflight() error = nil, want the bad target reported")
	}
	if !strings.Contains(err.Error(), "1/2") {
		t.Fatalf("Pool.Preflight() error = %v, want it to report 1/2 targets failed", err)
	}
}

// TestProbeDimension은 프리플라이트 프로브 크기가 디코더 핀을 따라가는지
// 본다. 핀이 꺼져 있으면 종전 640을 그대로 써야 한다 — 그렇지 않으면
// 머지만으로 부팅 동작이 바뀐다.
func TestProbeDimension(t *testing.T) {
	for _, tc := range []struct {
		name        string
		pinLongEdge int
		want        int
	}{
		{"핀 꺼짐", 0, preflightDim},
		{"핀 640", 640, 640},
		{"핀 1280", 1280, 1280},
		{"핀 1920", 1920, 1920},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := probeDimension(tc.pinLongEdge); got != tc.want {
				t.Fatalf("probeDimension(%d) = %d, want %d", tc.pinLongEdge, got, tc.want)
			}
		})
	}
}

// TestSyntheticFrameMatchesProbeDimension은 프로브가 실제로 그 크기로
// 만들어지는지 확인한다. 핀보다 작은 프로브를 보내면 AI가 상한을 낮게 잡고
// 있어도 부팅이 조용히 통과해, 실방송 첫 프레임부터 전량 거부된다(#122).
func TestSyntheticFrameMatchesProbeDimension(t *testing.T) {
	for _, dim := range []int{preflightDim, 1280, 1920} {
		data, err := syntheticFrame(dim)
		if err != nil {
			t.Fatalf("syntheticFrame(%d): %v", dim, err)
		}
		config, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("프로브 디코드 실패: %v", err)
		}
		if config.Width != dim || config.Height != dim {
			t.Fatalf("프로브 = %dx%d, want %dx%d", config.Width, config.Height, dim, dim)
		}
	}
}
