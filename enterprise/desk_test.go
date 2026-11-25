package enterprise

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTouchSessionThrottlesWrites(t *testing.T) {
	svc := testService(t)
	ctx := t.Context()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	svc.Clock = func() time.Time { return now }
	actor := svc.mustSession(t, UserAnalyst)

	var previousXmin string
	if err := svc.DB.QueryRowContext(ctx, `
		SELECT xmin::text FROM ent_sessions WHERE id=$1`, actor.SessionID).Scan(&previousXmin); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		elapsed, lastWrite time.Duration
		written            bool
	}{
		{30 * time.Second, 0, false},
		{119 * time.Second, 0, false},
		{2 * time.Minute, 2 * time.Minute, true},
		{3 * time.Minute, 2 * time.Minute, false},
		{5 * time.Minute, 5 * time.Minute, true},
		{5 * time.Minute, 5 * time.Minute, false},
	} {
		now = start.Add(step.elapsed)
		got, err := svc.TouchSession(ctx, actor.SessionID)
		if err != nil || got.ID != actor.ID || got.SessionID != actor.SessionID {
			t.Fatalf("touch at %s: actor=%+v err=%v", step.elapsed, got, err)
		}
		var xmin string
		var lastSeen, idle, absolute time.Time
		if err := svc.DB.QueryRowContext(ctx, `
			SELECT xmin::text, last_seen, idle_expires, absolute_expires
			FROM ent_sessions WHERE id=$1`, actor.SessionID).Scan(&xmin, &lastSeen, &idle, &absolute); err != nil {
			t.Fatal(err)
		}
		wantLastSeen := start.Add(step.lastWrite)
		if (xmin != previousXmin) != step.written || !lastSeen.Equal(wantLastSeen) ||
			!idle.Equal(wantLastSeen.Add(IdleTimeout)) || !absolute.Equal(start.Add(AbsoluteTimeout)) {
			t.Fatalf("touch at %s: xmin=%s previous=%s want write=%t last_seen=%v idle=%v absolute=%v",
				step.elapsed, xmin, previousXmin, step.written, lastSeen, idle, absolute)
		}
		previousXmin = xmin
	}
}

func TestTouchSessionChecksCurrentAccess(t *testing.T) {
	svc := testService(t)
	ctx := t.Context()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	svc.Clock = func() time.Time { return now }

	for _, tc := range []struct {
		elapsed time.Duration
		code    string
	}{
		{IdleTimeout + time.Second, "session_idle"},
		{AbsoluteTimeout + time.Second, "session_absolute"},
		{time.Minute, "session_ended"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			now = start
			actor := svc.mustSession(t, UserAnalyst)
			now = start.Add(tc.elapsed)
			if tc.code == "session_ended" {
				if err := svc.Logout(ctx, actor.SessionID); err != nil {
					t.Fatal(err)
				}
			}
			_, err := svc.TouchSession(ctx, actor.SessionID)
			var denied *Fail
			if !errors.As(err, &denied) || denied.Status != 401 || denied.Code != tc.code {
				t.Fatalf("touch at %s: got %v, want %s", tc.elapsed, err, tc.code)
			}
		})
	}

	now = start
	actor := svc.mustSession(t, UserAnalyst)
	got, err := svc.TouchSession(ctx, actor.SessionID)
	if err != nil || got.Gate() != "ready" {
		t.Fatalf("touch before disabling: actor=%+v err=%v", got, err)
	}
	if _, err := svc.DB.ExecContext(ctx, `UPDATE ent_users SET directory_status='disabled' WHERE id=$1`, actor.ID); err != nil {
		t.Fatal(err)
	}
	now = start.Add(time.Minute)
	got, err = svc.TouchSession(ctx, actor.SessionID)
	if err != nil || got.Gate() != "disabled" {
		t.Fatalf("touch after disabling: actor=%+v err=%v", got, err)
	}
}

func TestDeskConnectsEmployeeReviewAndLeader(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	pack, err := svc.EnsureReviewPack(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.EnsureWorkRoster(ctx); err != nil {
		t.Fatal(err)
	}
	var queued bool
	if err := svc.DB.QueryRowContext(ctx, `SELECT queued FROM tasks WHERE id=$1`, pack.TaskID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued {
		t.Fatal("review task was queued before anyone opened it")
	}

	analyst := svc.mustSession(t, UserAnalyst)
	leader := svc.mustSession(t, UserBiz)
	reviewer := svc.mustSession(t, UserReviewer)
	memberB := svc.mustSession(t, UserMemberB)

	employeeHome, err := svc.Entry(ctx, analyst)
	if err != nil || employeeHome.Kind != "workbench" || employeeHome.Href != "/function/tasks" || employeeHome.ProjectName != "门户整改" {
		t.Fatalf("employee entry=%+v err=%v", employeeHome, err)
	}
	leaderHome, err := svc.Entry(ctx, leader)
	if err != nil || leaderHome.Kind != "conclusions" || leaderHome.Href != "/enterprise/conclusions" || leaderHome.Post != "leader" {
		t.Fatalf("leader entry=%+v err=%v", leaderHome, err)
	}
	reviewHome, err := svc.Entry(ctx, reviewer)
	if err != nil || reviewHome.Kind != "review" || reviewHome.Href != "/enterprise/todos" {
		t.Fatalf("reviewer entry=%+v err=%v", reviewHome, err)
	}
	analyst, err = svc.TouchSession(ctx, analyst.SessionID)
	if err != nil || analyst.SessionProject == "" {
		t.Fatalf("employee project was not stored: %+v %v", analyst, err)
	}
	if _, err := svc.TaskAccess(ctx, analyst, pack.TaskID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.TaskAccess(ctx, memberB, pack.TaskID, false); err == nil {
		t.Fatal("project B employee read project A task")
	}
	if _, err := svc.TaskAccess(ctx, leader, pack.TaskID, true); err == nil {
		t.Fatal("leader was allowed to edit the task")
	}
	if err := svc.DB.QueryRowContext(ctx, `SELECT queued FROM tasks WHERE id=$1`, pack.TaskID).Scan(&queued); err != nil || queued {
		t.Fatalf("opening access queued the task: %v %v", queued, err)
	}

	before, err := svc.Conclusions(ctx, leader)
	if err != nil || len(before) != 1 || strings.Contains(before[0].Conclusion, "本次结论由员工送审") {
		t.Fatalf("leader before=%+v err=%v", before, err)
	}
	original, err := svc.Download(ctx, leader, before[0].VersionID)
	if err != nil || original.Hash != before[0].FileHash {
		t.Fatalf("leader download err=%v hash=%s", err, original.Hash)
	}

	report, err := svc.OpenReport(ctx, analyst, pack.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Supplement(ctx, analyst, report.ID); err != nil {
		t.Fatal(err)
	}
	body := "登录失败提示过长。影响范围只在未登录访客，已登录会话不受影响。本次结论由员工送审。"
	saved, err := svc.SaveDraft(ctx, analyst, report.ID, body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Submit(ctx, analyst, saved.ID, saved.BodyHash); err != nil {
		t.Fatal(err)
	}
	during, err := svc.Conclusions(ctx, leader)
	if err != nil || len(during) != 1 || strings.Contains(during[0].Conclusion, "本次结论由员工送审") {
		t.Fatalf("draft leaked to leader: %+v err=%v", during, err)
	}
	same, err := svc.Download(ctx, leader, before[0].VersionID)
	if err != nil || same.Hash != original.Hash {
		t.Fatalf("issued file changed before the new issue: %v %s", err, same.Hash)
	}

	issued, err := svc.Issue(ctx, reviewer, saved.ID, saved.BodyHash)
	if err != nil || issued.Status != "issued" {
		t.Fatalf("issue=%+v err=%v", issued, err)
	}
	after, err := svc.Conclusions(ctx, leader)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, item := range after {
		if item.VersionID == saved.ID && strings.Contains(item.Conclusion, "本次结论由员工送审") && item.IssuerName == "陈溯" && item.TaskID == pack.TaskID {
			found = true
			if item.FileHash == "" || item.FileHash == original.Hash {
				t.Fatalf("new issue did not freeze a new file: %+v", item)
			}
		}
	}
	if !found {
		t.Fatalf("leader did not see the new conclusion: %+v", after)
	}
	still, err := svc.Download(ctx, leader, before[0].VersionID)
	if err != nil || still.Hash != original.Hash {
		t.Fatalf("old issued file changed: %v %s", err, still.Hash)
	}
}

func (s *Service) mustSession(t *testing.T, userID string) Actor {
	t.Helper()
	actor := s.as(t, userID)
	id, _, err := s.StartSession(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	actor.SessionID = id
	return actor
}
