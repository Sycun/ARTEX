package enterprise

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func testService(t *testing.T) *Service {
	t.Helper()
	dsn := "postgres://artex@127.0.0.1:55432/artex_enterprise_test?sslmode=disable"
	store, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	svc, err := New(store.DB, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := store.ExecContext(ctx, `DELETE FROM tasks WHERE remark = 'DEMO-RV-001'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExecContext(ctx, `
		DELETE FROM explorations e
		WHERE e.goal LIKE '%DEMO-RV-001%'
		  AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.exploration_id = e.id)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExecContext(ctx, `DELETE FROM assets WHERE url = 'https://login.portal.northstar.example/'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExecContext(ctx, `
		TRUNCATE ent_review_records, ent_issues, ent_versions, ent_report_sources, ent_finding_report,
		         ent_reports, ent_evidence_requests, ent_original_grants, ent_originals, ent_delegations,
		         ent_audit_delegations, ent_todos, ent_join_requests, ent_duties, ent_members, ent_asset_obs,
		         ent_project_tasks, ent_project_findings, ent_unclaimed, ent_cold_archives, ent_sample_loads, ent_audit,
		         ent_notifications, ent_sessions, ent_identities, ent_projects, ent_init_grants, ent_users,
		         findings
		RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	if err := svc.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	return svc
}

func (s *Service) as(t *testing.T, id string) Actor {
	t.Helper()
	user, err := s.userByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return Actor{User: user}
}

func TestEnterpriseRules(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	admin := svc.as(t, UserAdmin)
	sample, err := svc.ImportSample(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if sample.Findings != 1 || sample.Cases != 0 || sample.Issued != 0 || sample.Cold != 1 || sample.Unclaimed != 3 {
		t.Fatalf("sample counts = %+v", sample)
	}
	again, err := svc.ImportSample(ctx, admin)
	if err != nil || !again.Reused || again.Findings != 1 || again.FindingID != sample.FindingID {
		t.Fatalf("repeat import = %+v err=%v", again, err)
	}
	running, queued, err := svc.Runtime(ctx)
	if err != nil || running != 0 || queued != 0 {
		t.Fatalf("runtime before flow running=%d queued=%d err=%v", running, queued, err)
	}

	t.Run("concurrent joins stay separate and retry reuses", func(t *testing.T) {
		analyst := svc.as(t, UserAnalyst)
		reviewer := svc.as(t, UserReviewer)
		var first, second Receipt
		var err1, err2 error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			first, err1 = svc.SubmitJoin(ctx, analyst, "ns-a", "参加门户整改演示，password="+DemoSecret)
		}()
		go func() {
			defer wg.Done()
			second, err2 = svc.SubmitJoin(ctx, reviewer, "NS-A", "复核门户整改")
		}()
		wg.Wait()
		if err1 != nil || err2 != nil {
			t.Fatal(err1, err2)
		}
		if first.ID == second.ID || first.ReceiptNo == second.ReceiptNo {
			t.Fatalf("two employees shared a receipt: %+v %+v", first, second)
		}
		if strings.Contains(first.Purpose, DemoSecret) {
			t.Fatalf("join purpose leaked credential: %s", first.Purpose)
		}
		if _, err := svc.GetJoin(ctx, analyst, second.ID); err == nil {
			t.Fatal("analyst read the other employee's join request")
		}
		retry, err := svc.SubmitJoin(ctx, analyst, "NS-A", "再写一次")
		if err != nil || !retry.Reused || retry.ID != first.ID {
			t.Fatalf("retry = %+v err=%v", retry, err)
		}
		var todos int
		if err := svc.DB.QueryRowContext(ctx, `SELECT count(*) FROM ent_todos WHERE subject_id=$1 AND status='open'`, first.ID).Scan(&todos); err != nil {
			t.Fatal(err)
		}
		if todos != 1 {
			t.Fatalf("retry created extra todos: %d", todos)
		}
	})

	t.Run("approve and reject cannot both commit", func(t *testing.T) {
		owner := svc.as(t, UserOwnerA)
		delegate := svc.as(t, UserDelegate)
		req, err := svc.SubmitJoin(ctx, delegate, "NS-A", "并发裁决")
		if err != nil {
			t.Fatal(err)
		}
		var approved, rejected Receipt
		var errA, errB error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			approved, errA = svc.DecideJoin(ctx, owner, req.ID, "approve", "member")
		}()
		go func() {
			defer wg.Done()
			<-start
			rejected, errB = svc.DecideJoin(ctx, owner, req.ID, "reject", "")
		}()
		close(start)
		wg.Wait()
		if errA != nil || errB != nil {
			t.Fatal(errA, errB)
		}
		if approved.Status == rejected.Status && approved.Status == "pending" {
			t.Fatal("both decisions left the request pending")
		}
		final, err := svc.GetJoin(ctx, owner, req.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.Status != "approved" && final.Status != "rejected" {
			t.Fatalf("status=%s", final.Status)
		}
		var members int
		if err := svc.DB.QueryRowContext(ctx, `SELECT count(*) FROM ent_members WHERE user_id=$1`, UserDelegate).Scan(&members); err != nil {
			t.Fatal(err)
		}
		if final.Status == "approved" && members != 1 {
			t.Fatalf("approved but members=%d", members)
		}
		if final.Status == "rejected" && members != 0 {
			t.Fatalf("rejected but members=%d", members)
		}
		if _, err := svc.DB.ExecContext(ctx, `DELETE FROM ent_members WHERE user_id=$1`, UserDelegate); err != nil {
			t.Fatal(err)
		}
	})

	owner := svc.as(t, UserOwnerA)
	if _, err := svc.DecideJoin(ctx, owner, mustReceipt(t, svc, UserAnalyst), "approve", "member"); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddMember(ctx, owner, sample.ProjectAID, MemberInput{UserID: UserReviewer, Role: "member", Duties: []string{"review"}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddMember(ctx, owner, sample.ProjectAID, MemberInput{UserID: UserAuditor, Role: "reader", Duties: []string{"audit"}}); err != nil {
		t.Fatal(err)
	}
	ownerB := svc.as(t, UserOwnerB)
	if err := svc.AddMember(ctx, ownerB, sample.ProjectBID, MemberInput{UserID: UserMemberB, Role: "member"}); err != nil {
		t.Fatal(err)
	}

	analyst := svc.as(t, UserAnalyst)
	reviewer := svc.as(t, UserReviewer)
	report, err := svc.OpenReport(ctx, analyst, sample.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	againReport, err := svc.OpenReport(ctx, analyst, sample.FindingID)
	if err != nil || !againReport.Reused || againReport.ID != report.ID {
		t.Fatalf("second open = %+v err=%v", againReport, err)
	}
	body := "登录失败时，页面返回了过长的内部错误说明。本段不粘贴原文。"
	if _, err := svc.SaveDraft(ctx, analyst, report.ID, body); err != nil {
		t.Fatal(err)
	}
	submitted, err := svc.Submit(ctx, analyst, report.VersionID, Hash([]byte(body)))
	if err != nil || submitted.Status != "in_review" {
		t.Fatalf("submit=%+v err=%v", submitted, err)
	}
	replay, err := svc.Submit(ctx, analyst, report.VersionID, Hash([]byte(body)))
	if err != nil || !replay.Reused {
		t.Fatalf("resubmit=%+v err=%v", replay, err)
	}
	var reviewTodos int
	if err := svc.DB.QueryRowContext(ctx, `SELECT count(*) FROM ent_todos WHERE subject_id=$1 AND kind='report_review' AND status='open'`, report.VersionID).Scan(&reviewTodos); err != nil {
		t.Fatal(err)
	}
	if reviewTodos != 1 {
		t.Fatalf("review todos=%d", reviewTodos)
	}

	request, err := svc.RequestOriginal(ctx, analyst, report.ID, "核对演示发现")
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := svc.RejectOriginal(ctx, reviewer, request.ID, "演示报告以脱敏正文为准，不查看原件。password="+DemoSecret)
	if err != nil || rejected.Status != "rejected" || strings.Contains(rejected.Reason, DemoSecret) {
		t.Fatalf("reject=%+v err=%v", rejected, err)
	}
	seen, err := svc.GetEvidence(ctx, analyst, request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if seen.Status != "rejected" || strings.Contains(seen.Reason+seen.Purpose, DemoSecret) {
		t.Fatalf("applicant saw credential or wrong status: %+v", seen)
	}
	if seen.CredentialNotice == "" {
		t.Fatal("applicant was not told the credential was removed")
	}
	ownerSeen, err := svc.GetEvidence(ctx, owner, request.ID)
	if err != nil || strings.Contains(ownerSeen.Reason, DemoSecret) {
		t.Fatalf("owner unlocked credential: %+v err=%v", ownerSeen, err)
	}
	todos, err := svc.Todos(ctx, analyst)
	if err != nil {
		t.Fatal(err)
	}
	for _, todo := range todos {
		if strings.Contains(todo.Preview, DemoSecret) || strings.Contains(todo.Preview, "不查看原件") {
			t.Fatalf("todo preview leaked free text: %+v", todo)
		}
	}
	events, err := svc.Audit(ctx, svc.as(t, UserAuditor), sample.ProjectAID)
	if err != nil {
		t.Fatal(err)
	}
	blob := auditBlob(events)
	if strings.Contains(blob, DemoSecret) || strings.Contains(blob, "不查看原件") {
		t.Fatalf("audit leaked reason: %s", blob)
	}
	exported, err := svc.AuditExport(ctx, svc.as(t, UserAuditor), sample.ProjectAID)
	if err != nil || strings.Contains(auditBlob(exported), DemoSecret) {
		t.Fatal(err, auditBlob(exported))
	}
	leaked, err := svc.NotificationsContain(ctx, DemoSecret)
	if err != nil || leaked {
		t.Fatalf("notification leaked credential: %v %v", leaked, err)
	}
	if _, err := svc.ReadOriginal(ctx, analyst, "orig-demo-ns-001"); err == nil {
		t.Fatal("rejected applicant read the screenshot")
	}

	returned, err := svc.Return(ctx, reviewer, report.VersionID, Hash([]byte(body)), "请补一句影响范围：只影响登录失败提示，不影响已登录会话。")
	if err != nil || returned.SuccessorID == "" {
		t.Fatalf("return=%+v err=%v", returned, err)
	}
	var drafts int
	if err := svc.DB.QueryRowContext(ctx, `SELECT count(*) FROM ent_versions WHERE report_id=$1 AND status='draft'`, report.ID).Scan(&drafts); err != nil || drafts != 1 {
		t.Fatalf("drafts=%d err=%v", drafts, err)
	}
	repeatReturn, err := svc.Return(ctx, reviewer, report.VersionID, Hash([]byte(body)), "再退一次")
	if err != nil || !repeatReturn.Reused || repeatReturn.SuccessorID != returned.SuccessorID {
		t.Fatalf("repeat return=%+v err=%v", repeatReturn, err)
	}
	if err := svc.DB.QueryRowContext(ctx, `SELECT count(*) FROM ent_versions WHERE report_id=$1 AND status='draft'`, report.ID).Scan(&drafts); err != nil || drafts != 1 {
		t.Fatalf("repeat created drafts=%d", drafts)
	}

	third := svc.as(t, UserMemberB)
	if err := svc.AddMember(ctx, owner, sample.ProjectAID, MemberInput{UserID: UserMemberB, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	changed := body + "。"
	saved, err := svc.SaveDraft(ctx, third, report.ID, changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Submit(ctx, third, saved.ID, saved.BodyHash); err != nil {
		t.Fatal(err)
	}
	if err := svc.AddMember(ctx, owner, sample.ProjectAID, MemberInput{UserID: UserAnalyst, Role: "member", Duties: []string{"review"}}); err != nil {
		t.Fatal(err)
	}
	analyst = svc.as(t, UserAnalyst)
	if _, err := svc.Issue(ctx, analyst, saved.ID, saved.BodyHash); err == nil {
		t.Fatal("source contributor issued a changed inherited version")
	}
	view, err := svc.Report(ctx, reviewer, report.ID)
	if err != nil {
		t.Fatal(err)
	}
	var issued ActionResult
	for _, version := range view.Versions {
		if version.ID == saved.ID && version.Status == "in_review" {
			issued, err = svc.Issue(ctx, reviewer, version.ID, version.BodyHash)
		}
	}
	if err != nil || issued.Status != "issued" || issued.FileHash == "" {
		t.Fatalf("reviewer issue=%+v err=%v", issued, err)
	}
	file, err := svc.Download(ctx, analyst, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if Hash(file.Bytes) != issued.FileHash || strings.Contains(string(file.Bytes), DemoSecret) {
		t.Fatalf("download hash=%s want=%s", Hash(file.Bytes), issued.FileHash)
	}
	auditorFile, err := svc.Download(ctx, svc.as(t, UserAuditor), saved.ID)
	if err != nil || Hash(auditorFile.Bytes) != file.Hash {
		t.Fatalf("auditor download err=%v hash=%s", err, auditorFile.Hash)
	}
	events, err = svc.Audit(ctx, svc.as(t, UserAuditor), sample.ProjectAID)
	if err != nil || !strings.Contains(auditBlob(events), issued.FileHash) {
		t.Fatal("audit missing issued hash", err)
	}

	t.Run("delegation dies when the draft changes", func(t *testing.T) {
		var findingID int64
		if err := svc.DB.QueryRowContext(ctx, `
			INSERT INTO findings (name, severity, summary, evidence, status)
			VALUES ('另一条演示发现','low','只用于委派','','pending') RETURNING id`).Scan(&findingID); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.DB.ExecContext(ctx, `INSERT INTO ent_project_findings (project_id, finding_id) VALUES ($1,$2)`, sample.ProjectAID, findingID); err != nil {
			t.Fatal(err)
		}
		opened, err := svc.OpenReport(ctx, analyst, findingID)
		if err != nil {
			t.Fatal(err)
		}
		firstBody := "待委派的正文"
		firstView, err := svc.SaveDraft(ctx, analyst, opened.ID, firstBody)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.SaveDraft(ctx, reviewer, opened.ID, firstBody); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Submit(ctx, analyst, firstView.ID, Hash([]byte(firstBody))); err == nil {
			t.Fatal("submit succeeded without a qualified reviewer")
		}
		if _, err := svc.GrantDelegation(ctx, admin, DelegationInput{
			ProjectID: sample.ProjectAID, VersionID: firstView.ID, BodyHash: Hash([]byte(firstBody)),
			Action: "issue", GranteeID: UserDelegate, ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		edited := firstBody + "。"
		editedView, err := svc.SaveDraft(ctx, analyst, opened.ID, edited)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Submit(ctx, analyst, editedView.ID, editedView.BodyHash); err == nil {
			t.Fatal("old delegation signed changed content")
		}
		if _, err := svc.GrantDelegation(ctx, admin, DelegationInput{
			ProjectID: sample.ProjectAID, VersionID: editedView.ID, BodyHash: editedView.BodyHash,
			Action: "issue", GranteeID: UserDelegate, ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Submit(ctx, analyst, editedView.ID, editedView.BodyHash); err != nil {
			t.Fatal(err)
		}
		delegate := svc.as(t, UserDelegate)
		result, err := svc.Issue(ctx, delegate, editedView.ID, editedView.BodyHash)
		if err != nil || result.Status != "issued" {
			t.Fatalf("delegate issue=%+v err=%v", result, err)
		}
		var members int
		if err := svc.DB.QueryRowContext(ctx, `SELECT count(*) FROM ent_members WHERE project_id=$1 AND user_id=$2`, sample.ProjectAID, UserDelegate).Scan(&members); err != nil || members != 0 {
			t.Fatalf("delegate became a member: %d err=%v", members, err)
		}
		if _, err := svc.Download(ctx, delegate, editedView.ID); err == nil {
			t.Fatal("delegate downloaded the report")
		}
		if _, err := svc.Report(ctx, delegate, report.ID); err == nil {
			t.Fatal("delegate opened another report")
		}
	})

	t.Run("issue and withdraw are one decision", func(t *testing.T) {
		var findingID int64
		if err := svc.DB.QueryRowContext(ctx, `
			INSERT INTO findings (name, severity, summary, evidence, status)
			VALUES ('并发裁决发现','low','并发','','pending') RETURNING id`).Scan(&findingID); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.DB.ExecContext(ctx, `INSERT INTO ent_project_findings (project_id, finding_id) VALUES ($1,$2)`, sample.ProjectAID, findingID); err != nil {
			t.Fatal(err)
		}
		opened, err := svc.OpenReport(ctx, analyst, findingID)
		if err != nil {
			t.Fatal(err)
		}
		text := "并发冻结正文"
		view, err := svc.SaveDraft(ctx, analyst, opened.ID, text)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Submit(ctx, analyst, view.ID, view.BodyHash); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var issued, withdrawn ActionResult
		var errI, errW error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			issued, errI = svc.Issue(ctx, reviewer, view.ID, view.BodyHash)
		}()
		go func() {
			defer wg.Done()
			<-start
			withdrawn, errW = svc.Withdraw(ctx, analyst, view.ID, view.BodyHash)
		}()
		close(start)
		wg.Wait()
		success := 0
		if errI == nil && issued.Status == "issued" {
			success++
		}
		if errW == nil && withdrawn.Status == "withdrawn" {
			success++
		}
		if success != 1 {
			t.Fatalf("success=%d issue=%+v %v withdraw=%+v %v", success, issued, errI, withdrawn, errW)
		}
		var issuedN, withdrawnN, drafts int
		if err := svc.DB.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE status='issued'), count(*) FILTER (WHERE status='withdrawn'), count(*) FILTER (WHERE status='draft') FROM ent_versions WHERE report_id=$1`, opened.ID).Scan(&issuedN, &withdrawnN, &drafts); err != nil {
			t.Fatal(err)
		}
		if issuedN+withdrawnN != 1 || (issuedN == 1 && drafts != 0) || (withdrawnN == 1 && drafts != 1) {
			t.Fatalf("issued=%d withdrawn=%d drafts=%d", issuedN, withdrawnN, drafts)
		}
	})

	memberB := svc.as(t, UserMemberB)
	if _, err := svc.DB.ExecContext(ctx, `DELETE FROM ent_members WHERE project_id=$1 AND user_id=$2`, sample.ProjectAID, UserMemberB); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Finding(ctx, memberB, sample.FindingID); err == nil {
		t.Fatal("project B member read project A finding")
	}
	if _, err := svc.Report(ctx, memberB, report.ID); err == nil {
		t.Fatal("project B member read project A report")
	}
	if _, err := svc.Download(ctx, memberB, saved.ID); err == nil {
		t.Fatal("project B member downloaded project A report")
	}
	assets, err := svc.Assets(ctx, memberB, sample.ProjectBID)
	if err != nil || len(assets) != 1 || assets[0].Name != "portal.northstar.example" || !strings.Contains(assets[0].Observation, "季度巡检") {
		t.Fatalf("B assets=%+v err=%v", assets, err)
	}
	findings, err := svc.Findings(ctx, memberB, sample.ProjectBID)
	if err != nil || len(findings) != 0 {
		t.Fatalf("B findings=%+v err=%v", findings, err)
	}
	running, queued, err = svc.Runtime(ctx)
	if err != nil || running != 0 || queued != 0 {
		t.Fatalf("runtime after flow running=%d queued=%d err=%v", running, queued, err)
	}
}

func mustReceipt(t *testing.T, svc *Service, userID string) string {
	t.Helper()
	var id string
	if err := svc.DB.QueryRowContext(context.Background(), `SELECT id FROM ent_join_requests WHERE applicant_id=$1 AND status='pending'`, userID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func auditBlob(events []AuditEvent) string {
	var b strings.Builder
	for _, event := range events {
		b.WriteString(event.Reason)
		b.WriteString(event.Summary)
		b.WriteString(event.Note)
		b.WriteString(event.ContentHash)
	}
	return b.String()
}
