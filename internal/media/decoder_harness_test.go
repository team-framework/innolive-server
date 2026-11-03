//go:build decoder_harness

package media

// 디코더·미리보기 인코더 실측 하니스(#544). 실제 ffmpeg로 두 가지를 잰다.
//
//   - A1(TestDecoderHarness): 손실이 섞인 입력에서 디코더 감시(P0-4)가 멈춤을
//     알아채고 다시 띄우는지, 손실이 없거나 적을 때 괜히 다시 띄우지 않는지.
//   - B2(TestPreviewEncoderHarness): 미리보기 인코더(P1-6)가 원본 해상도·스레드
//     제한에서 몇 fps까지 내는지.
//
// 실행: go test -tags decoder_harness -run 'TestDecoderHarness|TestPreviewEncoderHarness' -v -timeout 30m ./internal/media
// 환경변수: HARNESS_FFMPEG(ffmpeg 경로), HARNESS_WIRE(raw|jpeg, 기본 raw),
// HARNESS_ENCODER_THREADS(기본 1), HARNESS_CONCURRENCY(예: "1,6,12"),
// HARNESS_SECONDS(A1 영상 길이, 기본 40).
//
// 운영 ffmpeg로 재려면 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -tags decoder_harness`로
// 만든 바이너리를 운영 이미지에 읽기 전용으로 붙여 `--network none --tmpfs /tmp:exec`로 띄운다.
// 로컬 ffmpeg 8.1은 손실 뒤 출력이 모자라 짝이 어긋나지만 운영 5.1은 1:1을 지킨다(11/03).
// 결과는 반드시 운영 이미지 값으로 판단한다.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

const harnessFPS = 30

func harnessFFmpeg() string {
	if path := os.Getenv("HARNESS_FFMPEG"); path != "" {
		return path
	}
	return "ffmpeg"
}

func harnessWire() config.WireFormat {
	if os.Getenv("HARNESS_WIRE") == "jpeg" {
		return config.WireFormatJPEG
	}
	return config.WireFormatRaw
}

func harnessInt(name string, fallback int) int {
	if value, err := strconv.Atoi(os.Getenv(name)); err == nil && value > 0 {
		return value
	}
	return fallback
}

func harnessConcurrency() []int {
	raw := os.Getenv("HARNESS_CONCURRENCY")
	if raw == "" {
		return []int{1, 6, 12}
	}
	var levels []int
	for _, part := range strings.Split(raw, ",") {
		if value, err := strconv.Atoi(strings.TrimSpace(part)); err == nil && value > 0 {
			levels = append(levels, value)
		}
	}
	return levels
}

// harnessSource는 카메라처럼 움직임과 센서 잡음이 있는 합성 영상이다.
func harnessSource(width, height, seconds int) []string {
	return []string{"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%dx%d:rate=%d,noise=alls=12:allf=t", width, height, harnessFPS), "-t", strconv.Itoa(seconds)}
}

func runHarnessFFmpeg(t *testing.T, args ...string) {
	t.Helper()
	command := exec.Command(harnessFFmpeg(), append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg fixture failed: %v\n%s", err, output)
	}
}

// ---- A1: 손실 디코드 ----

type harnessUnit struct {
	data []byte
	// nals는 H.264 액세스 유닛의 NAL 경계다. VP8은 비어 있다.
	nals [][]byte
	idr  bool
}

// generateH264Units는 휴대폰 업링크처럼 baseline·SPS/PPS 반복·AUD 없는 H.264를 만든다.
// zerolatency는 스레드마다 슬라이스를 나누므로 sliced-threads를 꺼 슬라이스 수를 고정한다.
func generateH264Units(t *testing.T, slices, seconds int) []harnessUnit {
	path := filepath.Join(t.TempDir(), "input.h264")
	args := append(harnessSource(1920, 1080, seconds),
		"-c:v", "libx264", "-profile:v", "baseline", "-preset", "veryfast", "-tune", "zerolatency",
		"-g", "60", "-keyint_min", "60", "-sc_threshold", "0", "-bf", "0",
		"-x264-params", fmt.Sprintf("slices=%d:sliced-threads=0:aud=0:repeat-headers=1", slices), "-f", "h264", path)
	runHarnessFFmpeg(t, args...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return splitH264AccessUnits(data)
}

// splitH264AccessUnits는 AUD 없는 Annex B를 액세스 유닛으로 나눈다. 앞 유닛에 VCL이
// 있는데 SPS·PPS·AUD가 오거나 first_mb_in_slice가 0인 슬라이스가 오면 새 유닛이다.
func splitH264AccessUnits(data []byte) []harnessUnit {
	var nals [][]byte
	for offset := 0; ; {
		start, size := annexBStartCodeAt(data, offset)
		if start < 0 {
			break
		}
		next, _ := annexBStartCodeAt(data, start+size)
		end := next
		if end < 0 {
			end = len(data)
		}
		nals = append(nals, data[start:end])
		if next < 0 {
			break
		}
		offset = next
	}
	var units []harnessUnit
	var current harnessUnit
	sawVCL := false
	flush := func() {
		if len(current.nals) > 0 {
			current.data = bytes.Join(current.nals, nil)
			units = append(units, current)
		}
		current, sawVCL = harnessUnit{}, false
	}
	for _, nal := range nals {
		header := nalHeaderIndex(nal)
		if header < 0 || header >= len(nal) {
			continue
		}
		kind := nal[header] & 0x1f
		vcl := kind == 1 || kind == 5
		firstSlice := vcl && header+1 < len(nal) && nal[header+1]&0x80 != 0
		if sawVCL && (kind == 7 || kind == 8 || kind == 9 || firstSlice) {
			flush()
		}
		current.nals = append(current.nals, nal)
		if kind == 5 {
			current.idr = true
		}
		if vcl {
			sawVCL = true
		}
	}
	flush()
	return units
}

func nalHeaderIndex(nal []byte) int {
	if len(nal) >= 4 && nal[2] == 1 {
		return 3
	}
	if len(nal) >= 5 && nal[3] == 1 {
		return 4
	}
	return -1
}

func generateVP8Units(t *testing.T, seconds int) []harnessUnit {
	path := filepath.Join(t.TempDir(), "input.ivf")
	args := append(harnessSource(1920, 1080, seconds),
		"-c:v", "libvpx", "-deadline", "realtime", "-cpu-used", "8", "-g", "60", "-b:v", "4M", "-f", "ivf", path)
	runHarnessFFmpeg(t, args...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 32 || string(data[:4]) != "DKIF" {
		t.Fatal("not an IVF file")
	}
	var units []harnessUnit
	for offset := int(binary.LittleEndian.Uint16(data[6:8])); offset+12 <= len(data); {
		size := int(binary.LittleEndian.Uint32(data[offset : offset+4]))
		payload := data[offset+12 : offset+12+size]
		units = append(units, harnessUnit{data: payload, idr: len(payload) > 0 && payload[0]&1 == 0})
		offset += 12 + size
	}
	return units
}

type harnessLoss struct {
	name string
	// rate는 손실 구간에서 프레임이 손상될 확률이다.
	rate float64
	// from·to는 손실 구간(초)이다. to가 0이면 영상 끝까지다.
	from, to float64
	// outages는 연속 손실이 시작하는 시각(초)이다. 시작마다 outageFrames장을 잇달아 잃는다.
	outages      []float64
	outageFrames int
	// outageWhole이면 연속 손실 구간의 프레임을 통째로 버린다. 아니면 다중 슬라이스는
	// 앞쪽 슬라이스를 잃은 조각으로 넘긴다.
	outageWhole bool
}

// 10/21 메모의 "4슬라이스에서 40장을 잃으면 141 입력에 126 출력"을 흉내 낸 긴 끊김이다.
var harnessOutages = []float64{5, 10, 15, 20, 25, 30}

var harnessLosses = []harnessLoss{
	{name: "none"},
	{name: "random5", rate: 0.05},
	{name: "burst30", rate: 0.30, from: 10, to: 30},
	{name: "outage40x6-drop", outages: harnessOutages, outageFrames: 40, outageWhole: true},
	{name: "outage40x6-partial", outages: harnessOutages, outageFrames: 40},
}

func (l harnessLoss) inOutage(index int) bool {
	for _, start := range l.outages {
		first := int(start * harnessFPS)
		if index >= first && index < first+l.outageFrames {
			return true
		}
	}
	return false
}

func (l harnessLoss) lossEnd() float64 {
	if len(l.outages) > 0 {
		return l.outages[len(l.outages)-1] + float64(l.outageFrames)/harnessFPS
	}
	return l.to
}

// damage는 서버 조립기(pion samplebuilder)가 RTP 손실 뒤 디코더에 넘기는 입력을 흉내 낸다.
// 조각이 빠진 프레임은 통째로 버려진다. 다만 H.264는 NAL마다 새 파티션 시작으로 보므로,
// 다중 슬라이스 프레임에서 앞쪽 슬라이스를 잃으면 뒤쪽 슬라이스만 한 프레임으로 넘어온다.
// 다중 슬라이스는 두 경우를 반반으로, 단일 슬라이스와 VP8은 통째로 버린다. 키프레임도
// 손실을 받는다. ok가 false면 프레임을 버린 것이다.
// damagePolicy는 다중 슬라이스 프레임을 어떻게 잃을지다.
type damagePolicy int

const (
	// damageEither는 통째로 버리기와 앞쪽 슬라이스 잃기를 반반으로 고른다.
	damageEither damagePolicy = iota
	damageWhole
	damageLeading
)

func damage(unit harnessUnit, random *rand.Rand, policy damagePolicy) ([]byte, bool) {
	var slices []int
	for index, nal := range unit.nals {
		if header := nalHeaderIndex(nal); header >= 0 {
			if kind := nal[header] & 0x1f; kind == 1 || kind == 5 {
				slices = append(slices, index)
			}
		}
	}
	if len(slices) <= 1 || policy == damageWhole || policy == damageEither && random.Intn(2) == 0 {
		return nil, false
	}
	// 앞쪽 슬라이스 1..n-1개와 그 앞의 SPS·PPS를 잃고 나머지만 남는다.
	keepFrom := slices[1+random.Intn(len(slices)-1)]
	return bytes.Join(unit.nals[keepFrom:], nil), true
}

type decodeResult struct {
	fed, damaged, dropped, outputs int
	restarts, keyframeRequests     int64
	restartAt                      []float64
	maxGap                         time.Duration
	lossEndToSteady                time.Duration
	latencyHead, latencyTail       time.Duration
	tailFPS                        float64
}

func runDecodeHarness(t *testing.T, codec VideoCodec, units []harnessUnit, loss harnessLoss, seconds int) decodeResult {
	registry := metrics.New()
	transcoder := NewFFmpegTranscoder(harnessFFmpeg(), testLogger(), registry, TranscoderOptions{VideoCodec: codec, WireFormat: harnessWire()})
	started := time.Now()
	var spawned atomic.Int64
	var restartMu sync.Mutex
	var restartAt []float64
	transcoder.newCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if spawned.Add(1) > 1 {
			restartMu.Lock()
			restartAt = append(restartAt, time.Since(started).Seconds())
			restartMu.Unlock()
		}
		return exec.CommandContext(ctx, name, args...)
	}
	var requests atomic.Int64
	keyframes := newKeyframeWait(registry, config.PrivacyModeReal, func() { requests.Add(1) })

	input := make(chan frame, 8)
	output := make(chan frame, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- transcoder.DecodeStream(ctx, input, output, keyframes) }()

	type sample struct {
		at      time.Duration
		latency time.Duration
	}
	var samples []sample
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		for item := range output {
			now := time.Now()
			samples = append(samples, sample{at: now.Sub(started), latency: now.Sub(item.stageAt)})
		}
	}()

	result := decodeResult{}
	random := rand.New(rand.NewSource(538))
	lossTo := loss.to
	if lossTo == 0 {
		lossTo = float64(seconds) + 1
	}
	ticker := time.NewTicker(time.Second / harnessFPS)
	for index, unit := range units {
		<-ticker.C
		at := float64(index) / harnessFPS
		data := unit.data
		outage := loss.inOutage(index)
		if outage || loss.rate > 0 && at >= loss.from && at < lossTo && random.Float64() < loss.rate {
			policy := damageEither
			if outage && loss.outageWhole {
				policy = damageWhole
			} else if outage {
				policy = damageLeading
			}
			damaged, ok := damage(unit, random, policy)
			if !ok {
				result.dropped++
				continue
			}
			data = damaged
			result.damaged++
		}
		input <- frame{data: data, stageAt: time.Now()}
		result.fed++
	}
	ticker.Stop()
	// 남은 출력이 나올 시간을 준 뒤 끝낸다.
	time.Sleep(2 * time.Second)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("DecodeStream did not return after cancel")
	}
	close(output)
	<-collected

	result.outputs = len(samples)
	result.restarts = spawned.Load() - 1
	result.keyframeRequests = requests.Load()
	result.restartAt = restartAt
	videoEnd := time.Duration(float64(len(units)) / harnessFPS * float64(time.Second))
	// 영상이 끝난 뒤 취소로 밀려 나오는 마지막 출력은 간격에서 뺀다.
	for index := 1; index < len(samples) && samples[index].at < videoEnd+time.Second; index++ {
		if gap := samples[index].at - samples[index-1].at; gap > result.maxGap {
			result.maxGap = gap
		}
	}
	// 손실이 끝난 뒤 처음으로 1초 동안 25장 이상 나온 시점까지.
	if end := loss.lossEnd(); end > 0 && (loss.rate > 0 || len(loss.outages) > 0) {
		lossEnd := time.Duration(end * float64(time.Second))
		for index := range samples {
			if samples[index].at < lossEnd {
				continue
			}
			count := 0
			for _, other := range samples[index:] {
				if other.at-samples[index].at >= time.Second {
					break
				}
				count++
			}
			if count >= 25 {
				result.lossEndToSteady = samples[index].at - lossEnd
				break
			}
		}
	}
	median := func(from, to time.Duration) time.Duration {
		var values []time.Duration
		for _, s := range samples {
			if s.at >= from && s.at < to {
				values = append(values, s.latency)
			}
		}
		if len(values) == 0 {
			return 0
		}
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		return values[len(values)/2]
	}
	result.latencyHead = median(2*time.Second, 7*time.Second)
	result.latencyTail = median(videoEnd-5*time.Second, videoEnd)
	tail := 0
	for _, s := range samples {
		if s.at >= videoEnd-5*time.Second && s.at < videoEnd {
			tail++
		}
	}
	result.tailFPS = float64(tail) / 5
	return result
}

func TestDecoderHarness(t *testing.T) {
	seconds := harnessInt("HARNESS_SECONDS", 40)
	inputs := []struct {
		name  string
		codec VideoCodec
		units func(*testing.T) []harnessUnit
	}{
		{"H264-1slice", VideoCodecH264, func(t *testing.T) []harnessUnit { return generateH264Units(t, 1, seconds) }},
		{"H264-4slice", VideoCodecH264, func(t *testing.T) []harnessUnit { return generateH264Units(t, 4, seconds) }},
		{"VP8", VideoCodecVP8, func(t *testing.T) []harnessUnit { return generateVP8Units(t, seconds) }},
	}
	var report []string
	for _, input := range inputs {
		units := input.units(t)
		idrs, vcl := 0, 0
		for _, unit := range units {
			if unit.idr {
				idrs++
			}
			for _, nal := range unit.nals {
				if header := nalHeaderIndex(nal); header >= 0 && (nal[header]&0x1f == 1 || nal[header]&0x1f == 5) {
					vcl++
				}
			}
		}
		t.Logf("%s: %d frames, %d keyframes, %.1f slices/frame", input.name, len(units), idrs, float64(vcl)/float64(len(units)))
		for _, loss := range harnessLosses {
			name := input.name + "/" + loss.name
			t.Run(name, func(t *testing.T) {
				r := runDecodeHarness(t, input.codec, units, loss, seconds)
				restarts := make([]string, len(r.restartAt))
				for index, at := range r.restartAt {
					restarts[index] = fmt.Sprintf("%.1fs", at)
				}
				line := fmt.Sprintf("%-20s fed=%d damaged=%d dropped=%d out=%d deficit=%d restarts=%d %v kf_req=%d max_gap=%s steady_after_loss=%s latency_head=%s latency_tail=%s tail_fps=%.1f",
					name, r.fed, r.damaged, r.dropped, r.outputs, r.fed-r.outputs, r.restarts, restarts, r.keyframeRequests,
					r.maxGap.Round(10*time.Millisecond), r.lossEndToSteady.Round(10*time.Millisecond),
					r.latencyHead.Round(time.Millisecond), r.latencyTail.Round(time.Millisecond), r.tailFPS)
				t.Log(line)
				report = append(report, line)
			})
		}
	}
	t.Logf("A1 결과 (ffmpeg=%s, wire=%s)\n%s", harnessFFmpeg(), harnessWire(), strings.Join(report, "\n"))
}

// ---- B2: 미리보기 인코더 처리량 ----

func generateRawFrames(t *testing.T, width, height, count int) [][]byte {
	path := filepath.Join(t.TempDir(), "frames.yuv")
	args := append(harnessSource(width, height, 10), "-frames:v", strconv.Itoa(count), "-pix_fmt", "yuv420p", "-f", "rawvideo", path)
	runHarnessFFmpeg(t, args...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	size := rawFrameSize(uint16(width), uint16(height))
	var frames [][]byte
	for offset := 0; offset+size <= len(data); offset += size {
		frames = append(frames, data[offset:offset+size])
	}
	return frames
}

func generateJPEGFrames(t *testing.T, width, height, count int) [][]byte {
	dir := t.TempDir()
	args := append(harnessSource(width, height, 10), "-frames:v", strconv.Itoa(count), "-q:v", "3", filepath.Join(dir, "f%04d.jpg"))
	runHarnessFFmpeg(t, args...)
	var frames [][]byte
	for index := 1; index <= count; index++ {
		data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("f%04d.jpg", index)))
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, data)
	}
	return frames
}

// encodeSession은 프레임을 최대 속도로 미리보기 인코더에 넣고 초당 출력 수를 돌려준다.
func encodeSession(codec VideoCodec, threads, width, height int, frames [][]byte, total int) (float64, error) {
	transcoder := NewFFmpegTranscoder(harnessFFmpeg(), testLogger(), metrics.New(), TranscoderOptions{VideoCodec: codec, WireFormat: harnessWire(), EncoderThreads: threads})
	input := make(chan frame, 1)
	output := make(chan frame, 64)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- transcoder.EncodeStream(ctx, input, output)
		close(output)
	}()
	var outputs int
	var first, last time.Time
	counted := make(chan struct{})
	go func() {
		defer close(counted)
		for range output {
			now := time.Now()
			if outputs == 0 {
				first = now
			}
			last = now
			outputs++
		}
	}()
	for index := 0; index < total; index++ {
		input <- frame{data: frames[index%len(frames)], width: uint16(width), height: uint16(height), stageAt: time.Now()}
	}
	close(input)
	err := <-done
	<-counted
	if outputs < 2 {
		return 0, fmt.Errorf("encoder produced %d frames: %v", outputs, err)
	}
	// 첫 출력 이후 구간으로 잰다. 인코더 기동 시간을 빼야 정상 상태 처리량이다.
	return float64(outputs-1) / last.Sub(first).Seconds(), nil
}

func TestPreviewEncoderHarness(t *testing.T) {
	threads := harnessInt("HARNESS_ENCODER_THREADS", 1)
	const total = 240
	var report []string
	for _, size := range []struct{ width, height int }{{1280, 720}, {1920, 1080}} {
		var frames [][]byte
		if harnessWire() == config.WireFormatRaw {
			frames = generateRawFrames(t, size.width, size.height, 60)
		} else {
			frames = generateJPEGFrames(t, size.width, size.height, 60)
		}
		for _, codec := range []VideoCodec{VideoCodecVP8, VideoCodecH264} {
			for _, concurrency := range harnessConcurrency() {
				name := fmt.Sprintf("%s-%dp-x%d", codec, size.height, concurrency)
				t.Run(name, func(t *testing.T) {
					rates := make([]float64, concurrency)
					errs := make([]error, concurrency)
					var wg sync.WaitGroup
					for index := range concurrency {
						wg.Add(1)
						go func() {
							defer wg.Done()
							rates[index], errs[index] = encodeSession(codec, threads, size.width, size.height, frames, total)
						}()
					}
					wg.Wait()
					for _, err := range errs {
						if err != nil {
							t.Fatal(err)
						}
					}
					sort.Float64s(rates)
					line := fmt.Sprintf("%-16s threads=%d per_session_fps min=%.1f median=%.1f max=%.1f", name, threads, rates[0], rates[len(rates)/2], rates[len(rates)-1])
					t.Log(line)
					report = append(report, line)
				})
			}
		}
	}
	t.Logf("B2 결과 (ffmpeg=%s, wire=%s)\n%s", harnessFFmpeg(), harnessWire(), strings.Join(report, "\n"))
}
