package enterprise

import (
	"context"
	"strings"
	"testing"
)

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
