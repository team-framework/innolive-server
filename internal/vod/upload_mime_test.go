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
		{"video/mov", true},
		{"application/mp4", true},
		{"video/x-m4v", true},
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

func TestAcceptedUploadPartFilenameFallback(t *testing.T) {
	if !acceptedUploadPart("video/MOV", "CLIP.MOV") {
		t.Fatal("video/MOV + .MOV rejected")
	}
	if !acceptedUploadPart("application/zip", "clip.mov") {
		t.Fatal("weird MIME with .mov filename rejected")
	}
	if acceptedUploadPart("image/jpeg", "photo.jpg") {
		t.Fatal("jpeg accepted")
	}
	if !acceptedUploadPart("", "IMG_0001.MOV") {
		t.Fatal("empty MIME + .MOV rejected")
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

func TestAcceptedProbeCodecs(t *testing.T) {
	for _, name := range []string{"h264", "hevc", "h265", "HEVC"} {
		if !acceptedProbeVideoCodec(name) {
			t.Fatalf("video %q rejected", name)
		}
	}
	if acceptedProbeVideoCodec("mpeg4") || acceptedProbeVideoCodec("vp9") {
		t.Fatal("unexpected video codec accepted")
	}
	for _, name := range []string{"aac", "pcm_s16le", "alac", "mp3"} {
		if !acceptedProbeAudioCodec(name) {
			t.Fatalf("audio %q rejected", name)
		}
	}
	if acceptedProbeAudioCodec("speex") {
		t.Fatal("unexpected audio codec accepted")
	}
}

func TestUploadAcceptsQuickTimeMIME(t *testing.T) {
	s := testService(t)
	s.Probe = fakeProbe{duration: 3, err: nil}
	for _, tc := range []struct{ mime, name string }{
		{"video/quicktime", "clip.mov"},
		{"video/x-quicktime", "clip.mov"},
		{"", "clip.mov"},
		{"video/MOV", "CLIP.MOV"},
		{"application/zip", "phone.MOV"},
	} {
		t.Run(tc.mime+"/"+tc.name, func(t *testing.T) {
			body, contentType := uploadBodyWithMIME(t, 32, tc.mime, tc.name)
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
