package media

// fallbackBlurColumns·Rows는 fail-closed 블러가 화면을 나누는 격자다. 블러 강도를
// 픽셀 반경이 아니라 화면 비율로 정해, 카메라에 바짝 댄 얼굴처럼 화면을 크게
// 채운 대상도 해상도와 무관하게 알아볼 수 없게 한다. 합성 이미지 실측에서 32×18은
// 화면 높이 20%의 글씨가 일부 읽혔고 16×9는 읽히지 않았다(#463).
const (
	fallbackBlurColumns = 16
	fallbackBlurRows    = 9
)

// blurRawFrame은 yuv420p 프레임 전체를 격자 평균으로 줄였다가 bilinear로 다시
// 키운 흐린 프레임을 돌려준다. 출력은 격자 칸의 평균값만으로 정해지므로 원본
// 세부를 되살릴 수 없다. 크기가 맞지 않으면 false다 — 호출자는 원본 대신 검은
// 프레임을 내보내야 한다.
func blurRawFrame(frame []byte, width, height uint16) ([]byte, bool) {
	size := rawFrameSize(width, height)
	if size <= 0 || len(frame) != size {
		return nil, false
	}
	w, h := int(width), int(height)
	cw, ch := (w+1)/2, (h+1)/2
	luma, chroma := w*h, cw*ch
	blurred := make([]byte, size)
	blurPlane(blurred[:luma], frame[:luma], w, h)
	blurPlane(blurred[luma:luma+chroma], frame[luma:luma+chroma], cw, ch)
	blurPlane(blurred[luma+chroma:], frame[luma+chroma:], cw, ch)
	return blurred, true
}

// blurPlane은 한 평면을 블러한다. 평균은 2픽셀 간격으로 표본을 뽑는다. 출력이
// 칸 평균에만 의존하므로 표본을 줄여도 가림 강도는 같고 연산은 1/4이 된다.
// 확대는 8비트 고정소수점 bilinear다.
func blurPlane(dst, src []byte, w, h int) {
	gw, gh := min(fallbackBlurColumns, w), min(fallbackBlurRows, h)
	columnOf := make([]int, w)
	for x := range columnOf {
		columnOf[x] = x * gw / w
	}
	sums := make([]uint32, gw*gh)
	counts := make([]uint32, gw*gh)
	for y := 0; y < h; y += 2 {
		cell := (y * gh / h) * gw
		row := src[y*w : (y+1)*w]
		for x := 0; x < w; x += 2 {
			sums[cell+columnOf[x]] += uint32(row[x])
			counts[cell+columnOf[x]]++
		}
	}
	grid := make([]int32, gw*gh)
	for i := range grid {
		if counts[i] > 0 {
			grid[i] = int32(sums[i] / counts[i])
		}
	}

	x0s, x1s, xWeights := bilinearTaps(w, gw)
	y0s, y1s, yWeights := bilinearTaps(h, gh)
	rowMix := make([]int32, gw)
	for y := 0; y < h; y++ {
		top, bottom, weight := grid[y0s[y]*int32(gw):], grid[y1s[y]*int32(gw):], yWeights[y]
		for i := range rowMix {
			rowMix[i] = top[i]<<8 + (bottom[i]-top[i])*weight
		}
		out := dst[y*w : (y+1)*w]
		for x := range out {
			left, right := rowMix[x0s[x]], rowMix[x1s[x]]
			out[x] = byte((left<<8 + (right-left)*xWeights[x] + 1<<15) >> 16)
		}
	}
}

// bilinearTaps는 출력 좌표마다 이웃한 격자 칸 둘과 뒤쪽 칸의 가중치(0~256)를
// 돌려준다. 칸 중심 기준이라 가장자리 칸은 늘여 쓴다.
func bilinearTaps(size, cells int) (first, second, weight []int32) {
	first, second, weight = make([]int32, size), make([]int32, size), make([]int32, size)
	for i := 0; i < size; i++ {
		position := (float64(i)+0.5)*float64(cells)/float64(size) - 0.5
		if position < 0 {
			position = 0
		}
		lower := min(int(position), cells-1)
		upper := min(lower+1, cells-1)
		first[i], second[i] = int32(lower), int32(upper)
		weight[i] = int32((position - float64(lower)) * 256)
	}
	return first, second, weight
}
