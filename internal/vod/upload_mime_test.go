package vod

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"testing"

	"github.com/google/uuid"
)

func TestAcceptedUploadContentType(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"", true},
		{"video/mp4", true},
		{"VIDEO/MP4", true},
		{"video/mp4; charset=binary", true},
		{"video/quicktime", true},
		{"video/x-quicktime", true},
		{"application/octet-stream", true},
		{"video/webm", false},
		{"image/jpeg", false},
		{"video/mp4, video/quicktime", false},
		{"not a mime", false},
	}
	for _, tc := range cases {
		if got := acceptedUploadContentType(tc.raw); got != tc.want {
			t.Fatalf("%q: got %v want %v", tc.raw, got, tc.want)
		}
	}
}

func TestAcceptedProbeContainer(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"mov,mp4,m4a,3gp,3g2,mj2", true},
		{"mp4", true},
		{"mov", true},
		{"matroska,webm", false},
		{"avi", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := acceptedProbeContainer(tc.name); got != tc.want {
			t.Fatalf("%q: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestUploadAcceptsQuickTimeMIME(t *testing.T) {
	s := testService(t)
	s.Probe = fakeProbe{duration: 3, err: nil}
	for _, mimeType := range []string{"video/quicktime", "video/x-quicktime", ""} {
		t.Run(mimeType, func(t *testing.T) {
			body, contentType := uploadBodyWithMIME(t, 32, mimeType, "clip.mov")
			w := request(t, handler(s), http.MethodPost, "/vod/jobs", uuid.New(), body, contentType)
			if w.Code != 201 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	body, contentType := uploadBodyWithMIME(t, 32, "video/webm", "clip.webm")
	w := request(t, handler(s), http.MethodPost, "/vod/jobs", uuid.New(), body, contentType)
	if w.Code != 415 {
		t.Fatalf("webm status=%d body=%s", w.Code, w.Body.String())
	}
}

func uploadBodyWithMIME(t *testing.T, n int, partMIME, filename string) (*bytes.Buffer, string) {
	t.Helper()
	b := new(bytes.Buffer)
	m := multipart.NewWriter(b)
	headers := textproto.MIMEHeader{}
	headers.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
	if partMIME != "" {
		headers.Set("Content-Type", partMIME)
	}
	p, err := m.CreatePart(headers)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Write(bytes.Repeat([]byte("x"), n)); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	return b, m.FormDataContentType()
}
