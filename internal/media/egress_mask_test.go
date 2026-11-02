package media

import (
	"errors"
	"strings"
	"testing"

	"inno-live-server/internal/config"
)

// FFmpeg·네트워크 오류는 출력 URL을 그대로 싣지 않을 수 있다(다른 호스트로
// 리다이렉트, 키만 출력). URL 전체가 일치할 때만 가리면 키가 상태 응답과 로그로
// 나간다(P1-24).
func TestSanitizeErrorMasksStreamKeyOutsideFullURL(t *testing.T) {
	e := newTestEgress(config.WireFormatJPEG, "rtmps://a.rtmps.youtube.com/live2/abcd-efgh-ijkl-mnop")
	for _, cause := range []string{
		"rtmps://a.rtmps.youtube.com/live2/abcd-efgh-ijkl-mnop: Connection refused",
		"redirected to rtmp://b.rtmp.youtube.com/live2/abcd-efgh-ijkl-mnop",
		"invalid stream key abcd-efgh-ijkl-mnop",
	} {
		got := e.sanitizeError(errors.New(cause))
		if strings.Contains(got, "abcd-efgh-ijkl-mnop") {
			t.Errorf("sanitizeError(%q) = %q, still exposes the key", cause, got)
		}
	}
}
