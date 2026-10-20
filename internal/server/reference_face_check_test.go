package server

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// restart는 워커 프로세스 재시작을 흉내 낸다. 화이트리스트는 메모리에만 있어 모두
// 사라진다.
func (w *fakeAIWorker) restart() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.entries = nil
}

func (w *fakeAIWorker) dropEntry(entryID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.entries = slices.DeleteFunc(w.entries, func(entry string) bool { return entry == entryID })
}

func (w *fakeAIWorker) setStatusErr(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.statusErr = err
}

func (w *fakeAIWorker) failDelete(entryID string, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.deleteErrs == nil {
		w.deleteErrs = make(map[string]error)
	}
	w.deleteErrs[entryID] = err
}

// postReregistration은 face_id와 사진 contents를 올려 그 얼굴을 다시 등록한다.
func postReregistration(t *testing.T, baseURL, faceID string, contents ...string) *http.Response {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("face_id", faceID); err != nil {
		t.Fatal(err)
	}
	for _, content := range contents {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", `form-data; name="images"; filename="face.jpg"`)
		header.Set("Content-Type", "image/jpeg")
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(baseURL+"/reference-face", writer.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func reregistrationMarks(status referenceStatus) []bool {
	marks := make([]bool, 0, len(status.Faces))
	for _, face := range status.Faces {
		marks = append(marks, face.ReregistrationRequired)
	}
	return marks
}

func TestJudgeReferenceFace(t *testing.T) {
	face := referenceFace{FaceID: "face", EntryIDs: map[string]string{"a": "a-1", "b": "b-1"}}
	tests := []struct {
		name         string
		face         referenceFace
		observed     map[string]map[string][]string
		wantMissing  []string
		wantComplete bool
	}{
		{
			name:         "every worker holds the entry",
			face:         face,
			observed:     map[string]map[string][]string{"a": {"user:1": {"a-0", "a-1"}}, "b": {"user:1": {"b-1"}}},
			wantComplete: true,
		},
		{
			name:         "restarted worker lost the entry",
			face:         face,
			observed:     map[string]map[string][]string{"a": {"user:1": {"a-1"}}, "b": {"user:1": {}}},
			wantMissing:  []string{"b"},
			wantComplete: true,
		},
		{
			// 워커가 풀에 새로 들어왔거나 워커별 id를 기록하기 전에 등록한 얼굴이다.
			name:         "worker without a recorded entry id",
			face:         referenceFace{FaceID: "legacy"},
			observed:     map[string]map[string][]string{"a": {"user:1": {"a-1"}}, "b": {"user:1": {"b-1"}}},
			wantMissing:  []string{"a", "b"},
			wantComplete: true,
		},
		{
			name:         "unanswered worker is unknown, not missing",
			face:         face,
			observed:     map[string]map[string][]string{"a": {"user:1": {"a-1"}}, "b": {}},
			wantComplete: false,
		},
		{
			name:         "answered worker missing while another is unknown",
			face:         face,
			observed:     map[string]map[string][]string{"a": {"user:1": {}}, "b": {}},
			wantMissing:  []string{"a"},
			wantComplete: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := judgeReferenceFace(test.face, "user:1", test.observed)
			if !slices.Equal(got.missing, test.wantMissing) || got.complete != test.wantComplete {
				t.Fatalf("verdict = (missing %v, complete %v), want (missing %v, complete %v)",
					got.missing, got.complete, test.wantMissing, test.wantComplete)
			}
		})
	}
}

// 2026-10-20 프로덕션: 워커 재시작으로 서버 기록 13명 중 11명의 얼굴이 모든 워커에서
// 사라졌는데 서버는 등록됨으로 보였다.
func TestReferenceFaceCheckMarksFacesLostOnWorkerRestart(t *testing.T) {
	application, httpServer, workers, storePath := newReferenceFaceTestApplication(t, 2)
	registered := uploadReferenceFaces(t, httpServer.URL, 2)

	application.checkReferenceFaces(context.Background())
	if status := getReferenceStatus(t, httpServer.URL); status.ReregistrationRequired || slices.Contains(reregistrationMarks(status), true) {
		t.Fatalf("status before restart = %+v, want no re-registration marks", status)
	}

	workers[1].restart()
	application.checkReferenceFaces(context.Background())

	status := getReferenceStatus(t, httpServer.URL)
	if !status.ReregistrationRequired || !slices.Equal(reregistrationMarks(status), []bool{true, true}) {
		t.Fatalf("status after restart = %+v, want both faces marked", status)
	}
	// 기존 계약은 그대로다. 표시는 등록 여부를 바꾸지 않는다.
	if !status.Registered || status.Count != 2 || status.Faces[0].FaceID != registered.Faces[0].FaceID {
		t.Fatalf("status after restart = %+v, want the registered faces unchanged", status)
	}
	for _, face := range newReferenceStore(storePath, false).faces["default"] {
		if !face.ReregistrationRequired {
			t.Fatalf("reloaded face %s is not marked, want the mark persisted", face.FaceID)
		}
	}
}

// 같은 사람이라도 얼굴을 잃은 워커에 배정되면 블러된다. 워커 하나에서만 빠져도 표시한다.
func TestReferenceFaceCheckMarksFaceMissingOnSomeWorkers(t *testing.T) {
	application, httpServer, workers, _ := newReferenceFaceTestApplication(t, 2)
	uploadReferenceFaces(t, httpServer.URL, 2)

	workers[0].dropEntry("worker0-1")
	application.checkReferenceFaces(context.Background())

	status := getReferenceStatus(t, httpServer.URL)
	if !status.ReregistrationRequired || !slices.Equal(reregistrationMarks(status), []bool{true, false}) {
		t.Fatalf("marks = %v (summary %v), want only the first face marked", reregistrationMarks(status), status.ReregistrationRequired)
	}
}

// 답하지 않는 워커는 판정 근거가 아니다. 그것만으로 표시하지도, 지우지도 않는다.
func TestReferenceFaceCheckIgnoresWorkerThatDoesNotAnswer(t *testing.T) {
	application, httpServer, workers, _ := newReferenceFaceTestApplication(t, 2)
	uploadReferenceFaces(t, httpServer.URL, 1)

	workers[1].setStatusErr(status.Error(codes.Unavailable, "worker down"))
	application.checkReferenceFaces(context.Background())
	if status := getReferenceStatus(t, httpServer.URL); status.ReregistrationRequired {
		t.Fatalf("status = %+v, want no mark from an unanswered worker", status)
	}

	workers[0].restart()
	application.checkReferenceFaces(context.Background())
	if status := getReferenceStatus(t, httpServer.URL); !status.ReregistrationRequired {
		t.Fatalf("status = %+v, want a mark from the answered worker that lost the face", status)
	}
}

func TestReferenceFaceCheckClearsMarkOnlyWhenEveryWorkerHoldsTheFace(t *testing.T) {
	application, httpServer, workers, _ := newReferenceFaceTestApplication(t, 2)
	registered := uploadReferenceFaces(t, httpServer.URL, 1)
	// 이전 대조가 잘못 표시한 상태다(예: 잠깐 빈 답을 준 워커). 두 워커 모두 얼굴을 갖고 있다.
	face, _ := application.references.face("default", registered.Faces[0].FaceID)
	if _, err := application.references.applyMarks([]referenceMark{{clientID: "default", face: face, required: true}}); err != nil {
		t.Fatal(err)
	}

	workers[1].setStatusErr(status.Error(codes.Unavailable, "worker down"))
	application.checkReferenceFaces(context.Background())
	if status := getReferenceStatus(t, httpServer.URL); !status.ReregistrationRequired {
		t.Fatalf("status = %+v, want the mark kept while a worker is unknown", status)
	}

	workers[1].setStatusErr(nil)
	application.checkReferenceFaces(context.Background())
	if status := getReferenceStatus(t, httpServer.URL); status.ReregistrationRequired {
		t.Fatalf("status = %+v, want the mark cleared once every worker holds the face", status)
	}
}

func TestReregisterFaceRestoresEntriesAndClearsMark(t *testing.T) {
	application, httpServer, workers, _ := newReferenceFaceTestApplication(t, 2)
	registered := uploadReferenceFaces(t, httpServer.URL, 2)
	faceID := registered.Faces[0].FaceID
	application.references.mu.Lock()
	application.references.faces["default"][0].Name = "민수"
	application.references.mu.Unlock()
	// worker0에는 이전 엔트리가 남고 worker1은 모두 잃었다.
	workers[1].restart()
	application.checkReferenceFaces(context.Background())

	response := postReregistration(t, httpServer.URL, faceID, "face-again")
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("re-registration status = %d, body = %s", response.StatusCode, data)
	}
	var result referenceStatus
	mustDecode(t, response.Body, &result)
	if result.Count != 2 || result.Faces[0].FaceID != faceID || result.Faces[0].Name != "민수" {
		t.Fatalf("faces = %+v, want the same face id and name in place", result.Faces)
	}
	if !slices.Equal(reregistrationMarks(result), []bool{false, true}) || !result.ReregistrationRequired {
		t.Fatalf("marks = %v (summary %v), want only the untouched face still marked", reregistrationMarks(result), result.ReregistrationRequired)
	}

	// 남아 있던 이전 엔트리는 지워진다. 남기면 얼굴을 지워도 그 워커가 계속 블러에서 뺀다.
	if got := workers[0].snapshot(); !slices.Equal(got, []string{"worker0-2", "worker0-3"}) {
		t.Fatalf("worker0 entries = %v, want the second face and the new entry", got)
	}
	if got := workers[1].snapshot(); !slices.Equal(got, []string{"worker1-3"}) {
		t.Fatalf("worker1 entries = %v, want only the new entry", got)
	}
	face, _ := application.references.face("default", faceID)
	entryIDs := make([]string, 0, len(face.EntryIDs))
	for _, entryID := range face.EntryIDs {
		entryIDs = append(entryIDs, entryID)
	}
	slices.Sort(entryIDs)
	if !slices.Equal(entryIDs, []string{"worker0-3", "worker1-3"}) {
		t.Fatalf("stored entry ids = %v, want the new entry on every worker", face.EntryIDs)
	}

	application.checkReferenceFaces(context.Background())
	if marks := reregistrationMarks(getReferenceStatus(t, httpServer.URL)); !slices.Equal(marks, []bool{false, true}) {
		t.Fatalf("marks after the next check = %v, want the re-registered face to stay clear", marks)
	}
}

func TestReplaceUploadClearsReregistrationMarks(t *testing.T) {
	application, httpServer, workers, _ := newReferenceFaceTestApplication(t, 2)
	uploadReferenceFaces(t, httpServer.URL, 2)
	workers[1].restart()
	application.checkReferenceFaces(context.Background())

	response := postReferenceFaces(t, httpServer.URL, []referenceUploadPart{{field: "image", filename: "face.jpg", contentType: "image/jpeg", content: "replacement"}})
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("replace status = %d, body = %s", response.StatusCode, data)
	}
	var result referenceStatus
	mustDecode(t, response.Body, &result)
	if result.Count != 1 || result.ReregistrationRequired || result.Faces[0].ReregistrationRequired {
		t.Fatalf("status after replace = %+v, want one unmarked face", result)
	}
}

func TestReregisterRejectsUnknownFaceAndMultipleImages(t *testing.T) {
	_, httpServer, workers, _ := newReferenceFaceTestApplication(t, 1)
	registered := uploadReferenceFaces(t, httpServer.URL, 1)

	unknown := postReregistration(t, httpServer.URL, "no-such-face", "face-again")
	unknown.Body.Close()
	if unknown.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown face_id status = %d, want 404", unknown.StatusCode)
	}
	multiple := postReregistration(t, httpServer.URL, registered.Faces[0].FaceID, "face-a", "face-b")
	multiple.Body.Close()
	if multiple.StatusCode != http.StatusBadRequest {
		t.Fatalf("face_id with two images status = %d, want 400", multiple.StatusCode)
	}
	if got := workers[0].snapshot(); !slices.Equal(got, []string{"worker0-1"}) {
		t.Fatalf("worker entries = %v, want the rejected requests to leave the worker untouched", got)
	}
}

// 이전 엔트리를 지우지 못하면 새 엔트리를 되돌리고, 일부 워커에서만 지워졌을 수 있어
// 곧바로 재등록 필요로 표시한다.
func TestReregisterRollsBackAndMarksWhenPreviousEntriesCannotBeDeleted(t *testing.T) {
	_, httpServer, workers, _ := newReferenceFaceTestApplication(t, 2)
	registered := uploadReferenceFaces(t, httpServer.URL, 1)
	workers[0].failDelete("worker0-1", status.Error(codes.Unavailable, "worker down"))

	response := postReregistration(t, httpServer.URL, registered.Faces[0].FaceID, "face-again")
	response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("re-registration status = %d, want 502", response.StatusCode)
	}
	if got := workers[0].snapshot(); !slices.Equal(got, []string{"worker0-1"}) {
		t.Fatalf("worker0 entries = %v, want the new entry rolled back", got)
	}
	if got := workers[1].snapshot(); len(got) != 0 {
		t.Fatalf("worker1 entries = %v, want the new entry rolled back", got)
	}
	status := getReferenceStatus(t, httpServer.URL)
	if !status.ReregistrationRequired || status.Faces[0].FaceID != registered.Faces[0].FaceID {
		t.Fatalf("status = %+v, want the original face kept and marked", status)
	}
}

// 대조는 기록을 읽은 뒤 워커에 묻는다. 그 사이 지워졌거나 다시 등록된 얼굴에 낡은
// 판정을 쓰면 방금 되살린 얼굴이 재등록 필요로 보인다.
func TestReferenceStoreApplyMarksSkipsFacesChangedSinceCheck(t *testing.T) {
	store := newReferenceStore(filepath.Join(t.TempDir(), "reference-faces.json"), false)
	store.faces = map[string][]referenceFace{"user:1": {
		{FaceID: "reregistered", EntryIDs: map[string]string{"a": "a-2"}},
		{FaceID: "unchanged", EntryIDs: map[string]string{"a": "a-3"}},
	}}

	changed, err := store.applyMarks([]referenceMark{
		{clientID: "user:1", face: referenceFace{FaceID: "reregistered", EntryIDs: map[string]string{"a": "a-1"}}, required: true},
		{clientID: "user:1", face: referenceFace{FaceID: "deleted", EntryIDs: map[string]string{"a": "a-0"}}, required: true},
		{clientID: "user:1", face: referenceFace{FaceID: "unchanged", EntryIDs: map[string]string{"a": "a-3"}}, required: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0].face.FaceID != "unchanged" {
		t.Fatalf("changed = %+v, want only the unchanged face", changed)
	}
	faces := store.faces["user:1"]
	if faces[0].ReregistrationRequired || !faces[1].ReregistrationRequired {
		t.Fatalf("faces = %+v, want only the unchanged face marked", faces)
	}
}

func TestRunReferenceFaceCheckRunsAtStartAndOnRequest(t *testing.T) {
	previous := referenceCheckInterval
	referenceCheckInterval = time.Hour
	t.Cleanup(func() { referenceCheckInterval = previous })
	application, httpServer, workers, _ := newReferenceFaceTestApplication(t, 1)
	registered := uploadReferenceFaces(t, httpServer.URL, 1)
	workers[0].restart()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		application.RunReferenceFaceCheck(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	marked := func() bool { return getReferenceStatus(t, httpServer.URL).ReregistrationRequired }
	waitFor(t, marked, "the first check did not run at start")

	// 주기는 한 시간이다. 표시를 지운 뒤 다시 생기면 요청이 대조를 돌린 것이다.
	face, _ := application.references.face("default", registered.Faces[0].FaceID)
	if _, err := application.references.applyMarks([]referenceMark{{clientID: "default", face: face, required: false}}); err != nil {
		t.Fatal(err)
	}
	application.RequestReferenceFaceCheck()
	waitFor(t, marked, "a requested check did not run")
}
