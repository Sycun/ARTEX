package enterprise

import (
	"context"
	"strings"
	"testing"
)

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
