package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/enterprise"
)

func TestEnterpriseColdArchiveStorage(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	ent, err := enterprise.New(m.pg.DB, m.dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{ctx: t.Context(), m: m, engine: NewEngine(m), ent: ent}
	task, err := m.CreateTask("cold archive fixture", "round trip", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID := mustTaskID(t, task.ID)
	t.Cleanup(func() {
		s.engine.StopTask(task.ID)
		if _, err := m.pg.Exec(`DELETE FROM task_archives WHERE task_id=$1`, taskID); err != nil {
			t.Error(err)
		}
		if err := m.pg.DeleteTask(taskID); err != nil {
			t.Error(err)
		}
	})
	if err := m.SetTaskPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, restore := range []bool{true, false} {
		job, err := s.m.pg.QueueTaskArchive(taskID)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.runOneTaskArchiveJob(); err != nil {
			t.Fatal(err)
		}
		job, err = s.m.pg.GetTaskArchive(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if job.ArchivePath != taskArchiveBlobKey(job) || job.SHA256 == "" {
			t.Fatalf("archive metadata: %+v", job)
		}
		info, err := ent.BlobStore().Stat(t.Context(), job.ArchivePath)
		if err != nil || info.Size == 0 || info.Size != job.CompressedSize {
			t.Fatalf("archive blob size=%d, metadata size=%d, err=%v", info.Size, job.CompressedSize, err)
		}
		if restore {
			_, err = s.m.pg.QueueTaskArchiveRestore(job.ID)
		} else {
			_, err = s.m.pg.QueueTaskArchiveDelete(job.ID)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := s.runOneTaskArchiveJob(); err != nil {
			t.Fatal(err)
		}
		if _, err := ent.BlobStore().Stat(t.Context(), job.ArchivePath); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("consumed archive remains reachable: %v", err)
		}
		restored, err := m.pg.GetTask(taskID)
		if err != nil || (restored != nil) != restore {
			t.Fatalf("task presence after consuming archive, restore=%t: task=%v err=%v", restore, restored, err)
		}
	}
}

func TestTaskArchiveAPIQueueListAndLimits(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	defer m.Close()
	task, err := m.CreateTask("archive api", "verify 202", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = m.pg.Exec(`DELETE FROM task_archives WHERE task_id=$1`, taskID)
		_ = m.pg.DeleteTask(taskID)
	}()

	// Start with an already-cancelled context so the background archive worker
	// exits before these route-contract assertions enqueue work.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dataDir := t.TempDir()
	s := New(ctx, m, dataDir, dataDir, dataDir)
	s.archiveWG.Wait()
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		s.Handler().ServeHTTP(recorder, req)
		return recorder
	}

	recorder := request(http.MethodPost, "/api/tasks/"+task.ID+"/archive", "{}")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("running archive status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if err := m.pg.SetPaused(taskID, true); err != nil {
		t.Fatal(err)
	}
	recorder = request(http.MethodPost, "/api/tasks/"+task.ID+"/archive", "{}")
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("archive queue status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var queued struct {
		ID    int64  `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &queued); err != nil {
		t.Fatal(err)
	}
	if queued.ID <= 0 || queued.State != "archive_queued" {
		t.Fatalf("unexpected queue response: %+v", queued)
	}

	recorder = request(http.MethodGet, "/api/task-archives?page=1&size=10&q="+task.ID, "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), fmt.Sprintf(`"task_id":%d`, taskID)) {
		t.Fatalf("archive list status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	ids := make([]string, 101)
	for index := range ids {
		ids[index] = strconv.Itoa(index + 1)
	}
	body, _ := json.Marshal(map[string]any{"task_ids": ids})
	recorder = request(http.MethodPost, "/api/tasks/archive/batch", string(body))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "100") {
		t.Fatalf("oversized batch status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
