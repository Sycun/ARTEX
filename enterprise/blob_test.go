package enterprise

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestEnterpriseBlobStorage(t *testing.T) {
	var mu sync.Mutex
	objects := map[string][]byte{}
	rejectIssue := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/enterprise/")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			if rejectIssue && strings.HasPrefix(key, "issues/") {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			objects[key] = data
		case http.MethodGet:
			data, exists := objects[key]
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if _, err := w.Write(data); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected S3 method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()
	t.Setenv("ARTEX_S3_ENDPOINT", srv.URL)
	t.Setenv("ARTEX_S3_BUCKET", "enterprise")
	t.Setenv("ARTEX_S3_REGION", "us-east-1")
	t.Setenv("ARTEX_S3_ACCESS_KEY", "fixture-key")
	t.Setenv("ARTEX_S3_SECRET_KEY", "fixture-secret")
	svc := testService(t)
	ctx := t.Context()
	sample, err := svc.ImportSample(ctx, svc.as(t, UserAdmin))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"h-body": "shared-body-demo", "h-traffic": "traffic-body-demo"} {
		got, err := svc.blobs.Get(ctx, key)
		if err != nil || string(got) != want {
			t.Fatalf("sample blob %s = %q, %v", key, got, err)
		}
	}
	owner, analyst, reviewer := svc.as(t, UserOwnerA), svc.as(t, UserAnalyst), svc.as(t, UserReviewer)
	for _, in := range []MemberInput{
		{UserID: analyst.ID, Role: "member"},
		{UserID: reviewer.ID, Role: "member", Duties: []string{"review"}},
	} {
		if err := svc.AddMember(ctx, owner, sample.ProjectAID, in); err != nil {
			t.Fatal(err)
		}
	}
	report, err := svc.OpenReport(ctx, analyst, sample.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	request, err := svc.RequestOriginal(ctx, analyst, report.ID, "核对记录")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApproveOriginal(ctx, reviewer, request.ID, OriginalApproval{Actions: []string{ViewOriginal, ViewCredential}}); err != nil {
		t.Fatal(err)
	}
	original, err := svc.ReadOriginal(ctx, analyst, "orig-demo-ns-001")
	if err != nil || !bytes.Contains(original, []byte(DemoSecret)) {
		t.Fatalf("approved blob, bytes=%d err=%v", len(original), err)
	}

	saved, err := svc.SaveDraft(ctx, analyst, report.ID, "确认登录提示影响范围。")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Submit(ctx, analyst, saved.ID, saved.BodyHash); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Issue(ctx, reviewer, saved.ID, saved.BodyHash); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("Issue during S3 failure = %v", err)
	}
	var status string
	var notifications int
	if err := svc.DB.QueryRowContext(ctx, `SELECT status,
		(SELECT count(*) FROM ent_notifications WHERE event_type='report_issued')
		FROM ent_versions WHERE id=$1`, saved.ID).Scan(&status, &notifications); err != nil || status != "in_review" || notifications != 0 {
		t.Fatalf("after failed Issue: status=%s notifications=%d err=%v", status, notifications, err)
	}
	mu.Lock()
	rejectIssue = false
	mu.Unlock()
	if _, err := svc.Issue(ctx, reviewer, saved.ID, saved.BodyHash); err != nil {
		t.Fatal(err)
	}
	var key string
	var inline []byte
	if err := svc.DB.QueryRowContext(ctx, `SELECT blob_key, file_bytes FROM ent_issues WHERE version_id=$1`, saved.ID).
		Scan(&key, &inline); err != nil || !strings.HasPrefix(key, "issues/") || inline != nil {
		t.Fatalf("issued storage: key=%q inline=%v err=%v", key, inline, err)
	}
	file, err := svc.Download(ctx, analyst, saved.ID)
	if err != nil || Hash(file.Bytes) != file.Hash {
		t.Fatalf("blob download hash=%s computed=%s err=%v", file.Hash, Hash(file.Bytes), err)
	}
	if _, err := svc.DB.ExecContext(ctx, `UPDATE ent_issues SET file_bytes='old-inline'::bytea WHERE version_id=$1`, saved.ID); err != nil {
		t.Fatal(err)
	}
	fromBlob, err := svc.Download(ctx, analyst, saved.ID)
	if err != nil || !bytes.Equal(fromBlob.Bytes, file.Bytes) {
		t.Fatalf("blob was not used with inline data present: %v", err)
	}
	if _, err := svc.DB.ExecContext(ctx, `UPDATE ent_issues SET file_bytes=$2, blob_key=NULL WHERE version_id=$1`, saved.ID, file.Bytes); err != nil {
		t.Fatal(err)
	}
	legacy, err := svc.Download(ctx, analyst, saved.ID)
	if err != nil || !bytes.Equal(legacy.Bytes, file.Bytes) {
		t.Fatalf("legacy download differed: %v", err)
	}
}

func TestLegacyOriginalRemainsRestricted(t *testing.T) {
	svc := testService(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(svc.Restricted, "original-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, link} {
		if data, err := svc.readOriginalBlob(t.Context(), path); len(data) != 0 || !errors.Is(err, fs.ErrInvalid) {
			t.Fatalf("unsafe legacy path %q returned %d bytes, err=%v", path, len(data), err)
		}
	}
}
