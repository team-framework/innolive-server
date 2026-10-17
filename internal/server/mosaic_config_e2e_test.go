package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/ai"
	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
	"inno-live-server/internal/origin"
	"inno-live-server/internal/session"

	pionmedia "github.com/pion/webrtc/v4/pkg/media"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// mosaicRecordingAI는 받은 프레임을 그대로 돌려주고 프레임마다 실린 mosaic_config를
// 남긴다. 값이 없던 프레임은 nil로 남는다.
type mosaicRecordingAI struct {
	aiv1.UnimplementedAiProcessorServer
	seen chan *aiv1.MosaicConfig
}

func (s mosaicRecordingAI) ProcessVideo(stream grpc.BidiStreamingServer[aiv1.VideoChunk, aiv1.ProcessedVideoChunk]) error {
	for {
		request, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		var mosaic *aiv1.MosaicConfig
		if request.MosaicConfig != nil {
			mosaic = proto.Clone(request.MosaicConfig).(*aiv1.MosaicConfig)
		}
		select {
		case s.seen <- mosaic:
		default:
		}
		if err := stream.Send(&aiv1.ProcessedVideoChunk{Data: request.Data, Timestamp: request.Timestamp, StatusMessage: "success"}); err != nil {
			return err
		}
	}
}

// TestMosaicConfigReachesAIEveryFrameThroughLivePipeline은 실제 WebRTC 트랙이 붙은
// real 모드 세션에서 API로 정한 설정이 AI 요청까지 가는지 본다. 트랙보다 먼저 정한
// 설정이 새 Processor에 실리고, 송출 영상 크기 기준 mask_margin이 AI 입력 픽셀로
// 바뀌며, 비우면 field 7 없이 돌아가는지 확인한다.
func TestMosaicConfigReachesAIEveryFrameThroughLivePipeline(t *testing.T) {
	frames := generateVP8Frames(t, 30)
	worker := mosaicRecordingAI{seen: make(chan *aiv1.MosaicConfig, 1024)}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	aiv1.RegisterAiProcessorServer(grpcServer, worker)
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)
	pool, err := ai.NewPool([]string{listener.Addr().String()}, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	cfg := testServerConfig()
	cfg.PrivacyMode = config.PrivacyModeReal
	// 픽스처는 320x180이다. 송출 640x360이면 송출 16px은 AI 입력 8px이다.
	cfg.EgressVideoSize = "640x360"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := metrics.New()
	manager, err := session.NewManager(cfg, logger, registry, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.CloseAll)
	origins, err := origin.NewConfig(false, []string{"https://client.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(New(cfg, logger, registry, manager, pool, origins, nil, nil).Handler())
	t.Cleanup(server.Close)

	id, owner, _ := createMosaicTestSession(t, server.URL, "server")
	url := server.URL + "/sessions/" + id + "/mosaic-config"
	if status, body := putMosaicConfig(t, url, owner, `{"pixel_size":4,"mask_margin":16}`); status != http.StatusOK {
		t.Fatalf("status=%d body=%v", status, body)
	}

	track, _ := connectTestPublisher(t, server.URL, id, owner)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		ticker := time.NewTicker(time.Second / 30)
		defer ticker.Stop()
		for index := 0; ; index++ {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := track.WriteSample(pionmedia.Sample{Data: frames[index%len(frames)], Duration: time.Second / 30}); err != nil {
					return
				}
			}
		}
	}()

	next := func() *aiv1.MosaicConfig {
		t.Helper()
		select {
		case mosaic := <-worker.seen:
			return mosaic
		case <-time.After(15 * time.Second):
			t.Fatal("no frame reached the AI worker")
			return nil
		}
	}
	for i := 0; i < 3; i++ {
		got := next()
		if got.GetPixelSize() != 4 || got.MaskMargin == nil || got.GetMaskMargin() != 8 || got.BlurRadius != nil {
			t.Fatalf("frame %d mosaic_config = %v, want pixel_size 4 and mask_margin 8", i, got)
		}
	}

	if status, body := putMosaicConfig(t, url, owner, `{}`); status != http.StatusOK {
		t.Fatalf("reset status=%d body=%v", status, body)
	}
	deadline := time.After(15 * time.Second)
	for {
		select {
		case got := <-worker.seen:
			if got == nil {
				// 비운 뒤 첫 nil 이후로는 다시 실리지 않아야 한다.
				for i := 0; i < 3; i++ {
					if again := next(); again != nil {
						t.Fatalf("reset frame carried mosaic_config %v", again)
					}
				}
				return
			}
		case <-deadline:
			t.Fatal("frames kept carrying mosaic_config after reset")
		}
	}
}
