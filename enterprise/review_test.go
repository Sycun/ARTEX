package enterprise

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestListTasksPaginationFiltersAndFindingCounts(t *testing.T) {
	svc := testService(t)
	ctx := t.Context()
	store := &db.DB{DB: svc.DB}
	if _, err := store.ExecContext(ctx, `
		INSERT INTO ent_projects (id, code, name, status)
		VALUES ('list-a','LIST-A','任务项目 A','active'), ('list-b','LIST-B','任务项目 B','active')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExecContext(ctx, `
		INSERT INTO ent_members (project_id, user_id, role) VALUES ('list-a',$1,'reader')`, UserReader); err != nil {
		t.Fatal(err)
	}

	addTask := func(project, name, description, goal, status string, completedAt *time.Time) int64 {
		t.Helper()
		task, err := store.CreateTaskWithOptions(description, goal, db.TaskCreateOptions{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := store.DeleteTaskCascade(task.ID, false, true); err != nil {
				t.Error(err)
			}
		})
		if _, err := store.ExecContext(ctx, `UPDATE tasks SET status=$2, completed_at=$3 WHERE id=$1`,
			task.ID, status, completedAt); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ExecContext(ctx, `
			INSERT INTO ent_project_tasks (project_id, task_id) VALUES ($1,$2)`, project, task.ID); err != nil {
			t.Fatal(err)
		}
		return task.ID
	}
	addFinding := func(taskID int64, projects ...string) {
		t.Helper()
		if _, err := store.ExecContext(ctx, `
			WITH finding AS (INSERT INTO findings (task_id) VALUES ($1) RETURNING id)
			INSERT INTO ent_project_findings (project_id, finding_id)
			SELECT project_id, finding.id
			FROM finding CROSS JOIN unnest($2::text[]) AS projects(project_id)`, taskID, projects); err != nil {
			t.Fatal(err)
		}
	}
	latest := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	earlier := latest.Add(-time.Hour)
	olderTask := addTask("list-a", "旧任务", "Portal description", "无关目标", "done", &earlier)
	namedTask := addTask("list-a", "Portal login", "无关描述", "无关目标", "done", &latest)
	tiedTask := addTask("list-a", "同时间任务", "无关描述", "Portal goal", "done", &latest)
	runningTask := addTask("list-a", "", "Portal scan", "无关目标", "running", nil)
	foreignTask := addTask("list-b", "Portal external", "无关描述", "无关目标", "done", &latest)
	addFinding(olderTask, "list-a")
	addFinding(namedTask, "list-a", "list-b")
	addFinding(namedTask, "list-a")
	addFinding(namedTask, "list-b")
	addFinding(namedTask)
	addFinding(runningTask, "list-a")
	addFinding(foreignTask, "list-b")

	type row struct {
		id       int64
		findings int
	}
	reader := svc.as(t, UserReader)
	for _, tc := range []struct {
		name              string
		query             TaskQuery
		total, page, size int
		want              []row
	}{
		{"default order", TaskQuery{}, 4, 1, 20, []row{{tiedTask, 0}, {namedTask, 2}, {olderTask, 1}, {runningTask, 1}}},
		{"first filtered page", TaskQuery{Text: "  PORTAL ", Status: "done", Page: 1, PageSize: 2}, 3, 1, 2, []row{{tiedTask, 0}, {namedTask, 2}}},
		{"second filtered page", TaskQuery{Text: "portal", Status: "done", Page: 2, PageSize: 2}, 3, 2, 2, []row{{olderTask, 1}}},
		{"past last page", TaskQuery{Text: "portal", Status: "done", Page: 3, PageSize: 2}, 3, 3, 2, nil},
		{"status only", TaskQuery{Status: "running", PageSize: 100}, 1, 1, 50, []row{{runningTask, 1}}},
		{"no match", TaskQuery{Text: "external"}, 0, 1, 20, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list, err := svc.ListTasks(ctx, reader, "list-a", tc.query)
			if err != nil {
				t.Fatal(err)
			}
			if list.ProjectID != "list-a" || list.ProjectName != "任务项目 A" || list.Total != tc.total ||
				list.Page != tc.page || list.PageSize != tc.size || list.Items == nil {
				t.Fatalf("unexpected task list: %+v", list)
			}
			got := make([]row, 0, len(list.Items))
			for _, item := range list.Items {
				got = append(got, row{item.ID, item.Findings})
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReviewPackStaysSeparateFromDemoSample(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	beforeRun, beforeQueue, err := svc.Runtime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, err := svc.EnsureReviewPack(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Reused || first.TaskID == 0 || first.FileHash == "" || first.Sample != ReviewSampleKey {
		t.Fatalf("pack=%+v", first)
	}
	second, err := svc.EnsureReviewPack(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Reused || second.TaskID != first.TaskID || second.FileHash != first.FileHash || second.VersionID != first.VersionID {
		t.Fatalf("repeat pack changed data: %+v vs %+v", first, second)
	}
	reader := svc.as(t, UserReader)
	home, err := svc.ProjectHome(ctx, reader, first.ProjectID)
	if err != nil || home.Role != "reader" || home.Tasks != 1 || home.Issued != 1 || home.Name != "门户整改" {
		t.Fatalf("home=%+v err=%v", home, err)
	}
	var updatedBefore string
	var status string
	var queued bool
	if err := svc.DB.QueryRowContext(ctx, `SELECT status, queued, updated_at::text FROM tasks WHERE id=$1`, first.TaskID).Scan(&status, &queued, &updatedBefore); err != nil {
		t.Fatal(err)
	}
	list, err := svc.ListTasks(ctx, reader, first.ProjectID, TaskQuery{Text: "登录页", Status: "done"})
	if err != nil || list.Total != 1 || list.Items[0].ID != first.TaskID || list.Items[0].StatusLabel != "已完成" {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	miss, err := svc.ListTasks(ctx, reader, first.ProjectID, TaskQuery{Text: "不存在的任务"})
	if err != nil || miss.Total != 0 || len(miss.Items) != 0 {
		t.Fatalf("empty search=%+v err=%v", miss, err)
	}
	detail, err := svc.Task(ctx, reader, first.ProjectID, first.TaskID)
	if err != nil || detail.Running || detail.Queued || len(detail.Assets) != 1 || len(detail.Findings) != 1 || !detail.Findings[0].Issued {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
	if detail.Findings[0].ReportID != first.ReportID {
		t.Fatalf("finding report=%s want %s", detail.Findings[0].ReportID, first.ReportID)
	}
	report, err := svc.Report(ctx, reader, first.ReportID)
	if err != nil || report.IssuedCount != 1 || report.TaskID != first.TaskID || !strings.Contains(report.SampleLabel, "DEMO-RV-001") {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if report.Actions["save"] || report.Actions["issue"] || !report.Actions["download"] {
		t.Fatalf("reader actions=%v", report.Actions)
	}
	file, err := svc.Download(ctx, reader, first.VersionID)
	if err != nil || file.Hash != first.FileHash || strings.Contains(string(file.Bytes), DemoSecret) || !strings.Contains(string(file.Bytes), "版本：v1") {
		t.Fatalf("download hash=%s err=%v body=%s", file.Hash, err, file.Bytes)
	}
	again, err := svc.Download(ctx, reader, first.VersionID)
	if err != nil || again.Hash != file.Hash {
		t.Fatalf("second download hash=%s err=%v", again.Hash, err)
	}
	var updatedAfter string
	if err := svc.DB.QueryRowContext(ctx, `SELECT status, queued, updated_at::text FROM tasks WHERE id=$1`, first.TaskID).Scan(&status, &queued, &updatedAfter); err != nil {
		t.Fatal(err)
	}
	if status != "done" || queued || updatedAfter != updatedBefore {
		t.Fatalf("browse changed task status=%s queued=%v before=%s after=%s", status, queued, updatedBefore, updatedAfter)
	}
	admin := svc.as(t, UserAdmin)
	if _, err := svc.ListTasks(ctx, admin, first.ProjectID, TaskQuery{}); err == nil {
		t.Fatal("platform admin listed project tasks")
	}
	sample, err := svc.ImportSample(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if sample.Findings != 1 || sample.Cases != 0 || sample.Issued != 0 || sample.Baseline.Issued != 0 {
		t.Fatalf("baseline polluted: %+v", sample)
	}
	if sample.Current.Issued < 1 || sample.Current.Tasks < 1 || sample.Current.Findings < 2 {
		t.Fatalf("current counts=%+v", sample.Current)
	}
	repeat, err := svc.ImportSample(ctx, admin)
	if err != nil || !repeat.Reused || repeat.Findings != 1 || repeat.Issued != 0 || repeat.Baseline.Issued != 0 {
		t.Fatalf("repeat import=%+v err=%v", repeat, err)
	}
	ownerB := svc.as(t, UserOwnerB)
	if _, err := svc.Task(ctx, ownerB, sample.ProjectBID, first.TaskID); err == nil {
		t.Fatal("project B opened project A task")
	}
	bList, err := svc.ListTasks(ctx, ownerB, sample.ProjectBID, TaskQuery{Text: "登录"})
	if err != nil || bList.Total != 0 {
		t.Fatalf("project B list=%+v err=%v", bList, err)
	}
	bFindings, err := svc.SearchFindings(ctx, ownerB, sample.ProjectBID, FindingQuery{Text: "历史审核"})
	if err != nil || bFindings.Total != 0 {
		t.Fatalf("project B findings=%+v err=%v", bFindings, err)
	}
	if _, err := svc.Download(ctx, ownerB, first.VersionID); err == nil {
		t.Fatal("project B downloaded project A report")
	}
	afterRun, afterQueue, err := svc.Runtime(ctx)
	if err != nil || afterRun != beforeRun || afterQueue != beforeQueue || afterRun != 0 || afterQueue != 0 {
		t.Fatalf("runtime before=%d/%d after=%d/%d err=%v", beforeRun, beforeQueue, afterRun, afterQueue, err)
	}
	third, err := svc.EnsureReviewPack(ctx)
	if err != nil || third.FileHash != first.FileHash || third.TaskID != first.TaskID {
		t.Fatalf("pack after import=%+v err=%v", third, err)
	}
}
