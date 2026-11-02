package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// 교체 업로드가 400으로 끝나면 기존 얼굴은 워커와 목록에 그대로 남아야 한다. 그러지
// 않으면 등록한 본인이 블러된다.
func TestReplaceUploadKeepsPreviousFacesWhenRejected(t *testing.T) {
	cases := []struct {
		name string
		part referenceUploadPart
	}{
		{name: "unsupported format", part: referenceUploadPart{field: "image", filename: "bad.txt", contentType: "text/plain", content: "nope"}},
		{name: "undecodable image", part: referenceUploadPart{field: "image", filename: "bad.jpg", contentType: "image/jpeg", content: "not an image", raw: true}},
		{name: "no face detected", part: referenceUploadPart{field: "image", filename: "reject.jpg", contentType: "image/jpeg", content: "REJECT"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			httpServer, workers, _ := newReferenceFaceTestServer(t, 2)
			previous := uploadReferenceFaces(t, httpServer.URL, 1)

			response := postReferenceFaces(t, httpServer.URL, []referenceUploadPart{tc.part})
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				data, _ := io.ReadAll(response.Body)
				t.Fatalf("replace status = %d, want 400; body = %s", response.StatusCode, data)
			}

			for index, worker := range workers {
				entries := worker.snapshot()
				want := fmt.Sprintf("worker%d-1", index)
				if len(entries) != 1 || entries[0] != want {
					t.Fatalf("worker %d entries = %v, want the previous %q", index, entries, want)
				}
			}
			status := getReferenceStatus(t, httpServer.URL)
			if status.Count != 1 || status.Faces[0].FaceID != previous.Faces[0].FaceID {
				t.Fatalf("status after rejected replace = %+v, want the previous face", status)
			}
		})
	}
}

// 서버가 디코드하지 못한 파일은 워커에 보내지 않고 거절한다.
func TestUploadRejectsUndecodableImageBeforeAI(t *testing.T) {
	httpServer, workers, _ := newReferenceFaceTestServer(t, 1)

	response := postReferenceFaces(t, httpServer.URL, []referenceUploadPart{
		{field: "images", filename: "bad.jpg", contentType: "image/jpeg", content: "not an image", raw: true},
	})
	defer response.Body.Close()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	data, _ := io.ReadAll(response.Body)
	_ = json.Unmarshal(data, &payload)
	if response.StatusCode != http.StatusBadRequest || payload.Error.Code != "invalid_image" {
		t.Fatalf("status = %d, body = %s; want 400 invalid_image", response.StatusCode, data)
	}
	if calls := workers[0].addCalls.Load(); calls != 0 {
		t.Fatalf("AddWhitelist calls = %d, want 0 for an undecodable file", calls)
	}
}

// 같은 사용자의 업로드는 한 번에 하나씩 워커에 등록한다. 겹치면 목록에 없는 엔트리가
// 워커에 남아 그 얼굴이 계속 블러에서 빠진다.
func TestConcurrentUploadsForSameClientAreSerialized(t *testing.T) {
	application, httpServer, workers, _ := newReferenceFaceTestApplication(t, 1)
	worker := workers[0]
	worker.addStarted = make(chan struct{})
	worker.allowAdd = make(chan struct{})
	// 실패로 일찍 끝나도 막힌 등록을 풀어 서버가 닫히게 한다.
	release := sync.OnceFunc(func() { close(worker.allowAdd) })
	t.Cleanup(release)

	statuses := make(chan int, 2)
	replace := func() {
		response := postReferenceFaces(t, httpServer.URL, []referenceUploadPart{
			{field: "image", filename: "face.jpg", contentType: "image/jpeg", content: "face"},
		})
		_ = response.Body.Close()
		statuses <- response.StatusCode
	}
	go replace()
	<-worker.addStarted
	go replace()
	time.Sleep(200 * time.Millisecond)
	if calls := worker.addCalls.Load(); calls != 1 {
		t.Fatalf("AddWhitelist calls while the first upload is in flight = %d, want 1", calls)
	}
	release()
	for range 2 {
		if code := <-statuses; code != http.StatusCreated {
			t.Fatalf("upload status = %d, want 201", code)
		}
	}

	application.references.mu.RLock()
	var recorded []string
	for _, face := range application.references.faces["default"] {
		for _, entryID := range face.EntryIDs {
			recorded = append(recorded, entryID)
		}
	}
	application.references.mu.RUnlock()
	entries := worker.snapshot()
	slices.Sort(recorded)
	slices.Sort(entries)
	if len(recorded) != 1 || !slices.Equal(entries, recorded) {
		t.Fatalf("worker entries = %v, recorded = %v; want one matching entry", entries, recorded)
	}
}

// 메타 저장이 실패하면 업로드를 실패로 알리고 이 요청의 워커 등록을 되돌린다. 성공으로
// 알리면 재시작 뒤 응답과 다른 목록이 복원된다.
func TestUploadFailsAndRollsBackWhenMetadataCannotBeSaved(t *testing.T) {
	application, httpServer, workers, storePath := newReferenceFaceTestApplication(t, 2)
	previous := uploadReferenceFaces(t, httpServer.URL, 1)
	breakReferenceStore(application, storePath)

	response := postReferenceFaces(t, httpServer.URL, []referenceUploadPart{
		{field: "images", filename: "face.jpg", contentType: "image/jpeg", content: "face"},
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, want 500; body = %s", response.StatusCode, data)
	}
	for index, worker := range workers {
		entries := worker.snapshot()
		want := fmt.Sprintf("worker%d-1", index)
		if len(entries) != 1 || entries[0] != want {
			t.Fatalf("worker %d entries = %v, want only the previous %q", index, entries, want)
		}
	}
	status := getReferenceStatus(t, httpServer.URL)
	if status.Count != 1 || status.Faces[0].FaceID != previous.Faces[0].FaceID {
		t.Fatalf("status after failed save = %+v, want the previous face", status)
	}
}

// 삭제도 메타 저장이 실패하면 실패로 알리고 목록을 되돌린다. 워커 쪽 삭제는 다시
// 시도해도 같은 결과라 재시도로 맞출 수 있다.
func TestDeleteFailsWhenMetadataCannotBeSaved(t *testing.T) {
	for _, byID := range []bool{false, true} {
		t.Run(fmt.Sprintf("by id %v", byID), func(t *testing.T) {
			application, httpServer, _, storePath := newReferenceFaceTestApplication(t, 1)
			registered := uploadReferenceFaces(t, httpServer.URL, 1)
			breakReferenceStore(application, storePath)

			url := httpServer.URL + "/reference-face"
			if byID {
				url += "/" + registered.Faces[0].FaceID
			}
			response := deleteReferenceFace(t, url)
			response.Body.Close()
			if response.StatusCode != http.StatusInternalServerError {
				t.Fatalf("delete status = %d, want 500", response.StatusCode)
			}
			if status := getReferenceStatus(t, httpServer.URL); status.Count != 1 {
				t.Fatalf("status after failed save = %+v, want the face kept", status)
			}

			restoreReferenceStore(application, storePath)
			response = deleteReferenceFace(t, url)
			response.Body.Close()
			if response.StatusCode != http.StatusNoContent {
				t.Fatalf("retried delete status = %d, want 204", response.StatusCode)
			}
			if faces := newReferenceStore(storePath, false).faces["default"]; len(faces) != 0 {
				t.Fatalf("saved faces after retry = %+v, want none", faces)
			}
		})
	}
}

// 두 슬롯은 같은 메타 파일을 쓴다. 드레인 중인 옛 슬롯이 저장해도 새 슬롯이 등록한
// 다른 사용자의 얼굴 메타는 남아야 한다.
func TestReferenceStoreSaveKeepsFacesSavedByOtherSlot(t *testing.T) {
	httpServer, _, storePath := newReferenceFaceTestServer(t, 1)
	oldSlot := newReferenceStore(storePath, false)
	registered := uploadReferenceFaces(t, httpServer.URL, 1)

	// 옛 슬롯의 게스트 세션이 끝나 그 게스트의 메타를 지운다.
	if err := oldSlot.deleteClient("guest-on-old-slot"); err != nil {
		t.Fatal(err)
	}

	faces := newReferenceStore(storePath, false).faces["default"]
	if len(faces) != 1 || faces[0].FaceID != registered.Faces[0].FaceID {
		t.Fatalf("saved faces after old slot save = %+v, want the face registered on the new slot", faces)
	}
}

// breakReferenceStore는 저장 경로를 일반 파일 아래로 옮겨 다음 저장을 실패하게 한다.
func breakReferenceStore(application *Server, storePath string) {
	application.references.mu.Lock()
	application.references.path = filepath.Join(storePath, "cannot-create-child")
	application.references.mu.Unlock()
}

func restoreReferenceStore(application *Server, storePath string) {
	application.references.mu.Lock()
	application.references.path = storePath
	application.references.mu.Unlock()
}
