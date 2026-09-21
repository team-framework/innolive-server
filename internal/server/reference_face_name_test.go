package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strings"
	"testing"
)

func TestReferenceFaceNamePersistsAndRejectsInvalidNameBeforeAI(t *testing.T) {
	server, workers, storePath := newReferenceFaceTestServer(t, 1)
	upload := func(name string) *http.Response {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.WriteField("name", name); err != nil {
			t.Fatal(err)
		}
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", `form-data; name="images"; filename="face.jpg"`)
		header.Set("Content-Type", "image/jpeg")
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte("face-0")); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		res, err := http.Post(server.URL+"/reference-face", writer.FormDataContentType(), &body)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	response := upload("  게스트 1  ")
	if response.StatusCode != 201 {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d body=%s", response.StatusCode, data)
	}
	var result referenceStatus
	mustDecode(t, response.Body, &result)
	response.Body.Close()
	if result.Faces[0].Name != "게스트 1" {
		t.Fatalf("name=%q", result.Faces[0].Name)
	}
	stored, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string][]referenceFace
	if err := json.Unmarshal(stored, &document); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, faces := range document {
		for _, face := range faces {
			if face.Name == "게스트 1" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("name missing from persisted metadata")
	}
	for _, name := range []string{strings.Repeat("가", 41), "이름\n다음"} {
		response = upload(name)
		response.Body.Close()
		if response.StatusCode != 400 {
			t.Fatalf("invalid name status=%d", response.StatusCode)
		}
	}
	if len(workers[0].snapshot()) != 1 {
		t.Fatal("invalid name reached AI worker")
	}
}
