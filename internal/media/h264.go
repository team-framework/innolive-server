package media

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"inno-live-server/internal/config"
)

const h264AnnexBStartCode = "\x00\x00\x00\x01"

// decodeH264Stream consumes complete Annex-B H.264 access units assembled
// from RTP. Parameter-only access units are retained until an IDR frame so
// FFmpeg always starts with SPS/PPS followed by a decodable picture.
func (t *FFmpegTranscoder) decodeH264Stream(ctx context.Context, input <-chan frame, output chan<- frame) error {
	bootstrap := h264Bootstrap{}
	var first frame
	var width, height uint16
	for {
		item, ok := <-input
		if !ok {
			return nil
		}
		data, idr, w, h := bootstrap.prepare(item.data)
		if len(data) == 0 || !idr || w == 0 || h == 0 {
			continue
		}
		first, first.data, width, height = item, data, w, h
		break
	}

	process, err := t.startH264Decoder(ctx)
	if err != nil {
		return err
	}
	defer process.close()
	// 디코더 ffmpeg는 출력 스트림 규격을 첫 프레임에 고정하고, 이후 입력
	// 해상도가 올라가도 그 값으로 되돌린다. 즉 이 한 줄이 세션 화질의
	// 상한을 기록한다(#122).
	t.logger.Info("H.264 decoder output locked",
		"width", width, "height", height, "wire_format", t.options.WireFormat)

	metadata := make(chan frame, 64)
	writeError := make(chan error, 1)
	go func() {
		defer close(metadata)
		defer process.stdin.Close()
		// observed*는 퍼블리셔가 보내오는 입력 해상도를 관측만 하기 위한 것으로,
		// 이 고루틴 안에서만 읽고 쓴다(공유 상태가 아니라 레이스가 없다).
		observedWidth, observedHeight := width, height
		write := func(item frame) error {
			item.width, item.height = width, height
			if _, err := process.stdin.Write(item.data); err != nil {
				return err
			}
			select {
			case metadata <- item:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := write(first); err != nil {
			writeError <- err
			return
		}
		for {
			select {
			case <-ctx.Done():
				writeError <- ctx.Err()
				return
			case item, ok := <-input:
				if !ok {
					writeError <- nil
					return
				}
				data, _, inputWidth, inputHeight := bootstrap.prepare(item.data)
				if len(data) == 0 {
					continue
				}
				// 새 SPS 치수를 frame 메타데이터에 반영하지는 않는다 — 디코더가
				// 출력을 위에서 고정한 규격으로 되돌리므로 실제 페이로드 크기는
				// 그대로이고, 메타데이터만 바꾸면 하위 단계와 어긋난다(#122).
				// 퍼블리셔가 어디까지 올라오는지 관측만 남긴다.
				if inputWidth != 0 && inputHeight != 0 &&
					(inputWidth != observedWidth || inputHeight != observedHeight) {
					t.logger.Info("H.264 input resolution changed",
						"from_width", observedWidth, "from_height", observedHeight,
						"to_width", inputWidth, "to_height", inputHeight,
						"decoder_locked_width", width, "decoder_locked_height", height)
					observedWidth, observedHeight = inputWidth, inputHeight
				}
				item.data = data
				if err := write(item); err != nil {
					writeError <- err
					return
				}
			}
		}
	}()

	rawSize := rawFrameSize(width, height)
	for item := range metadata {
		if t.options.WireFormat == config.WireFormatRaw {
			item.data, err = readRawFrame(process.stdout, rawSize)
		} else {
			item.data, err = readJPEG(process.stdout)
		}
		if err != nil {
			return fmt.Errorf("read decoded frame from FFmpeg H.264 decoder: %w", err)
		}
		select {
		case output <- item:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := <-writeError; err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("write H.264 stream to FFmpeg decoder: %w", err)
	}
	return nil
}

func (t *FFmpegTranscoder) encodeH264Stream(ctx context.Context, input <-chan frame, output chan<- frame) error {
	var first frame
	select {
	case <-ctx.Done():
		return ctx.Err()
	case item, ok := <-input:
		if !ok {
			return nil
		}
		first = item
	}
	process, err := t.startH264Encoder(ctx, first.width, first.height)
	if err != nil {
		return err
	}
	defer process.close()

	metadata := make(chan frame, 64)
	writeError := make(chan error, 1)
	go func() {
		defer close(metadata)
		defer process.stdin.Close()
		write := func(item frame) error {
			if err := t.validateEncoderInput(item.data, rawFrameSize(first.width, first.height)); err != nil {
				return err
			}
			if _, err := process.stdin.Write(item.data); err != nil {
				return err
			}
			select {
			case metadata <- item:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := write(first); err != nil {
			writeError <- err
			return
		}
		for {
			select {
			case <-ctx.Done():
				writeError <- ctx.Err()
				return
			case item, ok := <-input:
				if !ok {
					writeError <- nil
					return
				}
				if err := write(item); err != nil {
					writeError <- err
					return
				}
			}
		}
	}()

	reader := h264AccessUnitReader{reader: process.stdout}
	for item := range metadata {
		item.data, err = reader.Read()
		if err != nil {
			return fmt.Errorf("read H.264 access unit from FFmpeg encoder: %w", err)
		}
		select {
		case output <- item:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := <-writeError; err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("write frame stream to FFmpeg H.264 encoder: %w", err)
	}
	return nil
}

// startH264Decoder는 VP8 경로의 startDecoder와 동일한 저지연 인자를 쓴다.
// 이 인자들이 없으면 ffmpeg가 기본 probe 예산(5MB / 5초)을 채울 때까지
// 출력을 시작하지 않아 스폰부터 첫 프레임까지 1.7초가 걸린다(#130 실측).
// codec과 컨테이너를 이미 강제하므로 probe 자체가 필요 없다.
func (t *FFmpegTranscoder) startH264Decoder(ctx context.Context) (*ffmpegProcess, error) {
	arguments := []string{
		"-hide_banner", "-loglevel", "error",
		"-probesize", "32", "-analyzeduration", "0", "-fpsprobesize", "0", "-threads", "1",
		"-f", "h264", "-blocksize", "1024", "-i", "pipe:0",
	}
	if t.options.WireFormat == config.WireFormatRaw {
		arguments = append(arguments, "-an", "-f", "rawvideo", "-pix_fmt", "yuv420p", "-blocksize", "1024")
	} else {
		arguments = append(arguments, "-an", "-f", "image2pipe", "-vcodec", "mjpeg", "-q:v", "3", "-pix_fmt", "yuvj420p", "-blocksize", "1024")
	}
	arguments = append(arguments, "-flush_packets", "1", "pipe:1")
	process, err := t.startFFmpeg(ctx, "decoder", nil, nil, arguments...)
	if err != nil {
		return nil, fmt.Errorf("start FFmpeg H.264 decoder: %w", err)
	}
	return process, nil
}

// startH264Encoder도 VP8 경로의 startEncoder와 인자를 맞춘다(#130).
// 스레드는 VP8과 마찬가지로 자원 거버너(EncoderThreads)를 따른다.
func (t *FFmpegTranscoder) startH264Encoder(ctx context.Context, width, height uint16) (*ffmpegProcess, error) {
	arguments := []string{
		"-hide_banner", "-loglevel", "error",
		"-probesize", "32", "-analyzeduration", "0", "-fpsprobesize", "0",
	}
	if t.options.WireFormat == config.WireFormatRaw {
		arguments = append(arguments, "-f", "rawvideo", "-pixel_format", "yuv420p", "-video_size", fmt.Sprintf("%dx%d", width, height), "-framerate", "30", "-blocksize", "1024", "-i", "pipe:0")
	} else {
		arguments = append(arguments, "-f", "image2pipe", "-vcodec", "mjpeg", "-framerate", "30", "-blocksize", "1024", "-i", "pipe:0")
	}
	if t.options.EncoderThreads > 0 {
		arguments = append(arguments, "-threads", strconv.Itoa(t.options.EncoderThreads))
	}
	arguments = append(arguments,
		"-an", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-profile:v", "baseline", "-level:v", "3.1",
		"-pix_fmt", "yuv420p", "-g", "30", "-keyint_min", "30", "-sc_threshold", "0", "-bf", "0",
		"-x264-params", "repeat-headers=1:aud=1:annexb=1", "-f", "h264", "-flush_packets", "1", "pipe:1",
	)
	process, err := t.startFFmpeg(ctx, "encoder", nil, nil, arguments...)
	if err != nil {
		return nil, fmt.Errorf("start FFmpeg H.264 encoder: %w", err)
	}
	return process, nil
}

type h264Bootstrap struct {
	sps           []byte
	pps           []byte
	width, height uint16
}

func (b *h264Bootstrap) prepare(data []byte) ([]byte, bool, uint16, uint16) {
	nalus := splitAnnexBNALUs(data)
	if len(nalus) == 0 {
		return nil, false, b.width, b.height
	}
	hasVCL, hasIDR, hasSPS, hasPPS := false, false, false, false
	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}
		switch nalu[0] & 0x1f {
		case 7:
			hasSPS = true
			b.sps = append(b.sps[:0], nalu...)
			if width, height, err := h264SPSDimensions(nalu); err == nil {
				b.width, b.height = width, height
			}
		case 8:
			hasPPS = true
			b.pps = append(b.pps[:0], nalu...)
		case 1, 2, 3, 4, 5:
			hasVCL = true
			if nalu[0]&0x1f == 5 {
				hasIDR = true
			}
		}
	}
	if !hasVCL {
		return nil, false, b.width, b.height
	}
	if hasIDR && (!hasSPS || !hasPPS) && len(b.sps) > 0 && len(b.pps) > 0 {
		prefix := make([]byte, 0, len(h264AnnexBStartCode)*2+len(b.sps)+len(b.pps)+len(data))
		prefix = append(prefix, h264AnnexBStartCode...)
		prefix = append(prefix, b.sps...)
		prefix = append(prefix, h264AnnexBStartCode...)
		prefix = append(prefix, b.pps...)
		prefix = append(prefix, data...)
		data = prefix
	}
	return data, hasIDR, b.width, b.height
}

func splitAnnexBNALUs(data []byte) [][]byte {
	var result [][]byte
	for offset := 0; ; {
		start, size := annexBStartCodeAt(data, offset)
		if start < 0 {
			break
		}
		next, _ := annexBStartCodeAt(data, start+size)
		if next < 0 {
			next = len(data)
		}
		if start+size < next {
			result = append(result, data[start+size:next])
		}
		if next == len(data) {
			break
		}
		offset = next
	}
	return result
}

func annexBStartCodeAt(data []byte, offset int) (int, int) {
	for index := offset; index+3 <= len(data); index++ {
		if data[index] != 0 || data[index+1] != 0 {
			continue
		}
		if data[index+2] == 1 {
			return index, 3
		}
		if index+4 <= len(data) && data[index+2] == 0 && data[index+3] == 1 {
			return index, 4
		}
	}
	return -1, 0
}

type h264AccessUnitReader struct {
	reader *bufio.Reader
	buffer []byte
}

func (r *h264AccessUnitReader) Read() ([]byte, error) {
	for {
		starts := h264AUDStarts(r.buffer)
		if len(starts) >= 2 {
			result := append([]byte(nil), r.buffer[:starts[1]]...)
			r.buffer = append(r.buffer[:0], r.buffer[starts[1]:]...)
			return result, nil
		}
		if len(r.buffer) > maxEncodedFrameSize {
			return nil, errors.New("H.264 access unit exceeds maximum size")
		}
		value, err := r.reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) && len(r.buffer) > 0 {
				result := append([]byte(nil), r.buffer...)
				r.buffer = nil
				return result, nil
			}
			return nil, err
		}
		r.buffer = append(r.buffer, value)
	}
}

func h264AUDStarts(data []byte) []int {
	var starts []int
	for offset := 0; ; {
		start, size := annexBStartCodeAt(data, offset)
		if start < 0 || start+size >= len(data) {
			return starts
		}
		if data[start+size]&0x1f == 9 {
			starts = append(starts, start)
		}
		offset = start + size
	}
}

// h264SPSDimensions reads coded display dimensions from an SPS NAL unit. It
// only needs the sequence-parameter fields up to frame cropping; slice data is
// never parsed here.
func h264SPSDimensions(nalu []byte) (uint16, uint16, error) {
	if len(nalu) < 4 || nalu[0]&0x1f != 7 {
		return 0, 0, errors.New("invalid H.264 SPS")
	}
	reader := h264BitReader{data: removeH264EmulationPrevention(nalu[1:])}
	profileID, err := reader.readBits(8)
	if err != nil {
		return 0, 0, err
	}
	if _, err = reader.readBits(8); err != nil { // constraint flags + reserved bits
		return 0, 0, err
	}
	if _, err = reader.readBits(8); err != nil { // level_idc
		return 0, 0, err
	}
	if _, err = reader.readUE(); err != nil { // seq_parameter_set_id
		return 0, 0, err
	}
	chromaFormatIDC := uint32(1)
	if h264ExtendedProfile(profileID) {
		chromaFormatIDC, err = reader.readUE()
		if err != nil {
			return 0, 0, err
		}
		if chromaFormatIDC == 3 {
			if _, err = reader.readBits(1); err != nil {
				return 0, 0, err
			}
		}
		if _, err = reader.readUE(); err != nil { // bit_depth_luma_minus8
			return 0, 0, err
		}
		if _, err = reader.readUE(); err != nil { // bit_depth_chroma_minus8
			return 0, 0, err
		}
		if _, err = reader.readBits(1); err != nil { // qpprime_y_zero_transform_bypass_flag
			return 0, 0, err
		}
		scaling, err := reader.readBits(1)
		if err != nil {
			return 0, 0, err
		}
		if scaling != 0 {
			count := 8
			if chromaFormatIDC == 3 {
				count = 12
			}
			for index := 0; index < count; index++ {
				present, err := reader.readBits(1)
				if err != nil {
					return 0, 0, err
				}
				if present != 0 {
					size := 16
					if index >= 6 {
						size = 64
					}
					if err := reader.skipScalingList(size); err != nil {
						return 0, 0, err
					}
				}
			}
		}
	}
	if _, err = reader.readUE(); err != nil { // log2_max_frame_num_minus4
		return 0, 0, err
	}
	picOrderCntType, err := reader.readUE()
	if err != nil {
		return 0, 0, err
	}
	if picOrderCntType == 0 {
		if _, err = reader.readUE(); err != nil {
			return 0, 0, err
		}
	} else if picOrderCntType == 1 {
		if _, err = reader.readBits(1); err != nil {
			return 0, 0, err
		}
		if _, err = reader.readSE(); err != nil {
			return 0, 0, err
		}
		if _, err = reader.readSE(); err != nil {
			return 0, 0, err
		}
		count, err := reader.readUE()
		if err != nil {
			return 0, 0, err
		}
		for index := uint32(0); index < count; index++ {
			if _, err = reader.readSE(); err != nil {
				return 0, 0, err
			}
		}
	}
	if _, err = reader.readUE(); err != nil { // max_num_ref_frames
		return 0, 0, err
	}
	if _, err = reader.readBits(1); err != nil { // gaps_in_frame_num_value_allowed_flag
		return 0, 0, err
	}
	picWidthInMbsMinus1, err := reader.readUE()
	if err != nil {
		return 0, 0, err
	}
	picHeightInMapUnitsMinus1, err := reader.readUE()
	if err != nil {
		return 0, 0, err
	}
	frameMbsOnlyFlag, err := reader.readBits(1)
	if err != nil {
		return 0, 0, err
	}
	if frameMbsOnlyFlag == 0 {
		if _, err = reader.readBits(1); err != nil { // mb_adaptive_frame_field_flag
			return 0, 0, err
		}
	}
	if _, err = reader.readBits(1); err != nil { // direct_8x8_inference_flag
		return 0, 0, err
	}
	cropping, err := reader.readBits(1)
	if err != nil {
		return 0, 0, err
	}
	cropLeft, cropRight, cropTop, cropBottom := uint32(0), uint32(0), uint32(0), uint32(0)
	if cropping != 0 {
		if cropLeft, err = reader.readUE(); err != nil {
			return 0, 0, err
		}
		if cropRight, err = reader.readUE(); err != nil {
			return 0, 0, err
		}
		if cropTop, err = reader.readUE(); err != nil {
			return 0, 0, err
		}
		if cropBottom, err = reader.readUE(); err != nil {
			return 0, 0, err
		}
	}
	width := (picWidthInMbsMinus1 + 1) * 16
	height := (picHeightInMapUnitsMinus1 + 1) * 16 * (2 - frameMbsOnlyFlag)
	cropUnitX, cropUnitY := uint32(1), uint32(2-frameMbsOnlyFlag)
	if chromaFormatIDC != 0 {
		subWidthC, subHeightC := uint32(2), uint32(2)
		if chromaFormatIDC == 2 {
			subHeightC = 1
		} else if chromaFormatIDC == 3 {
			subWidthC, subHeightC = 1, 1
		}
		cropUnitX = subWidthC
		cropUnitY = subHeightC * (2 - frameMbsOnlyFlag)
	}
	if cropLeft+cropRight >= width/cropUnitX || cropTop+cropBottom >= height/cropUnitY {
		return 0, 0, errors.New("invalid H.264 SPS crop")
	}
	width -= (cropLeft + cropRight) * cropUnitX
	height -= (cropTop + cropBottom) * cropUnitY
	if width == 0 || height == 0 || width > 65535 || height > 65535 {
		return 0, 0, errors.New("invalid H.264 SPS dimensions")
	}
	return uint16(width), uint16(height), nil
}

func h264ExtendedProfile(profile uint32) bool {
	switch profile {
	case 44, 83, 86, 100, 110, 118, 122, 128, 134, 135, 138, 139, 244:
		return true
	default:
		return false
	}
}

func removeH264EmulationPrevention(data []byte) []byte {
	output := make([]byte, 0, len(data))
	zeros := 0
	for _, value := range data {
		if zeros >= 2 && value == 3 {
			zeros = 0
			continue
		}
		output = append(output, value)
		if value == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return output
}

type h264BitReader struct {
	data   []byte
	offset uint
}

func (r *h264BitReader) readBits(count uint) (uint32, error) {
	if count > 32 || r.offset+count > uint(len(r.data))*8 {
		return 0, io.ErrUnexpectedEOF
	}
	var value uint32
	for ; count > 0; count-- {
		value = value<<1 | uint32((r.data[r.offset/8]>>(7-r.offset%8))&1)
		r.offset++
	}
	return value, nil
}

func (r *h264BitReader) readUE() (uint32, error) {
	zeros := uint(0)
	for {
		bit, err := r.readBits(1)
		if err != nil {
			return 0, err
		}
		if bit != 0 {
			break
		}
		zeros++
		if zeros > 31 {
			return 0, errors.New("invalid H.264 Exp-Golomb value")
		}
	}
	suffix, err := r.readBits(zeros)
	if err != nil {
		return 0, err
	}
	return (1<<zeros - 1) + suffix, nil
}

func (r *h264BitReader) readSE() (int32, error) {
	value, err := r.readUE()
	if err != nil {
		return 0, err
	}
	if value&1 == 0 {
		return -int32(value / 2), nil
	}
	return int32((value + 1) / 2), nil
}

func (r *h264BitReader) skipScalingList(size int) error {
	lastScale, nextScale := int32(8), int32(8)
	for index := 0; index < size; index++ {
		if nextScale != 0 {
			delta, err := r.readSE()
			if err != nil {
				return err
			}
			nextScale = (lastScale + delta + 256) % 256
		}
		if nextScale != 0 {
			lastScale = nextScale
		}
	}
	return nil
}
