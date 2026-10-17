package ai

import (
	"context"
	"io"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// captureAIServer는 받은 요청의 wire 바이트를 남긴다. 생성 코드의 필드가 아니라
// 실제로 보낸 바이트에 field 7이 있는지 봐야 회귀를 잡는다.
type captureAIServer struct {
	aiv1.UnimplementedAiProcessorServer
	requests chan []byte
}

func (s captureAIServer) ProcessVideo(stream grpc.BidiStreamingServer[aiv1.VideoChunk, aiv1.ProcessedVideoChunk]) error {
	for {
		request, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		wire, err := proto.Marshal(request)
		if err != nil {
			return err
		}
		s.requests <- wire
		if err := stream.Send(&aiv1.ProcessedVideoChunk{Data: request.Data, Timestamp: request.Timestamp, StatusMessage: "success"}); err != nil {
			return err
		}
	}
}

func hasField(t *testing.T, wire []byte, number protowire.Number) bool {
	t.Helper()
	for len(wire) > 0 {
		field, kind, n := protowire.ConsumeTag(wire)
		if n < 0 {
			t.Fatalf("invalid tag: %v", protowire.ParseError(n))
		}
		wire = wire[n:]
		if field == number {
			return true
		}
		n = protowire.ConsumeFieldValue(field, kind, wire)
		if n < 0 {
			t.Fatalf("invalid field %d: %v", field, protowire.ParseError(n))
		}
		wire = wire[n:]
	}
	return false
}

func TestStreamProcessSendsMosaicConfigOnlyWhenSet(t *testing.T) {
	server := captureAIServer{requests: make(chan []byte, 4)}
	client := newStreamTestClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := client.NewStream(ctx, "session", false)
	defer stream.Close()

	if _, err := stream.Process([]byte("frame"), 1, 0, 0, "", nil); err != nil {
		t.Fatal(err)
	}
	if wire := <-server.requests; hasField(t, wire, 7) {
		t.Fatal("request without a mosaic config must not carry field 7")
	}

	margin := uint32(0)
	if _, err := stream.Process([]byte("frame"), 2, 0, 0, "", &aiv1.MosaicConfig{MaskMargin: &margin}); err != nil {
		t.Fatal(err)
	}
	var received aiv1.VideoChunk
	if err := proto.Unmarshal(<-server.requests, &received); err != nil {
		t.Fatal(err)
	}
	mosaic := received.GetMosaicConfig()
	if mosaic == nil || mosaic.MaskMargin == nil || mosaic.GetMaskMargin() != 0 {
		t.Fatalf("mosaic_config = %v, want explicit mask_margin 0", mosaic)
	}
	if mosaic.BlurRadius != nil || mosaic.PixelSize != nil {
		t.Fatalf("unset fields arrived as set: %v", mosaic)
	}
}
