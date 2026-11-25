package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/enterprise"
	"github.com/Autumn-27/artex/notify"
)

func TestEnterpriseNotificationDelivery(t *testing.T) {
	t.Setenv(notify.AllowLocalTargetsEnv, "1")
	s := newEnterpriseHTTPTestServer(t)
	ctx := t.Context()
	pg := s.m.pg
	want := map[string]int{
		"join_requested": 1, "member_added": 1, "original_requested": 1, "original_rejected": 1,
		"report_submitted": 2, "report_returned": 1, "report_issued": 1,
	}
	const deliveries = 8
	payloads := make(chan string, deliveries)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case payloads <- string(raw):
		default:
			t.Error("too many webhook requests")
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()
	defer unblock()

	var watermark int64
	if err := pg.QueryRowContext(ctx, `SELECT COALESCE(max(id),0) FROM notification_events`).Scan(&watermark); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.ExecContext(ctx, `UPDATE notification_events SET fanned_out=true WHERE NOT fanned_out AND id <= $1`, watermark); err != nil {
		t.Fatal(err)
	}
	channel := &db.NotificationChannel{
		Name: t.Name(), Kind: notify.KindWebhook, Mode: db.NotifyModeRealtime,
		Config: json.RawMessage(fmt.Sprintf(`{"url":%q}`, hook.URL)),
	}
	channelID, err := pg.SaveNotificationChannel(ctx, channel)
	if err != nil {
		t.Fatal(err)
	}
	channel.ID = channelID
	t.Cleanup(func() {
		if _, err := pg.Exec(`DELETE FROM notification_channels WHERE id=$1`, channelID); err != nil {
			t.Error(err)
		}
		if _, err := pg.Exec(`DELETE FROM notification_events WHERE id > $1 AND left(kind, 11)='enterprise_'`, watermark); err != nil {
			t.Error(err)
		}
	})

	actor := func(id, platformRole string) enterprise.Actor {
		return enterprise.Actor{User: enterprise.User{
			ID: id, PlatformRole: platformRole, DirectoryStatus: "verified", Employment: "active",
		}}
	}
	owner := actor(enterprise.UserOwnerA, "")
	reviewer := actor(enterprise.UserReviewer, "")
	analyst := actor(enterprise.UserAnalyst, "")
	sample, err := s.ent.ImportSample(ctx, actor(enterprise.UserAdmin, "admin"))
	if err != nil {
		t.Fatal(err)
	}
	const (
		joinPurpose     = "outbound-private-join-purpose"
		originalPurpose = "outbound-private-original-purpose"
		rejection       = "outbound-private-rejection"
		returnReason    = "outbound-private-return-reason"
		reportBody      = "outbound-private-report-body"
	)
	secret := " password=" + enterprise.DemoSecret
	join, err := s.ent.SubmitJoin(ctx, analyst, enterprise.ProjectA, joinPurpose+secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ent.DecideJoin(ctx, owner, join.ID, "approve", "member"); err != nil {
		t.Fatal(err)
	}
	if err := s.ent.AddMember(ctx, owner, sample.ProjectAID, enterprise.MemberInput{
		UserID: reviewer.ID, Role: "member", Duties: []string{"review"},
	}); err != nil {
		t.Fatal(err)
	}
	report, err := s.ent.OpenReport(ctx, analyst, sample.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	request, err := s.ent.RequestOriginal(ctx, analyst, report.ID, originalPurpose+secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ent.RejectOriginal(ctx, reviewer, request.ID, rejection+secret); err != nil {
		t.Fatal(err)
	}
	saved, err := s.ent.SaveDraft(ctx, analyst, report.ID, reportBody)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ent.Submit(ctx, analyst, saved.ID, saved.BodyHash); err != nil {
		t.Fatal(err)
	}
	returned, err := s.ent.Return(ctx, reviewer, saved.ID, saved.BodyHash, returnReason+secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ent.Submit(ctx, analyst, returned.SuccessorID, saved.BodyHash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ent.Issue(ctx, reviewer, returned.SuccessorID, saved.BodyHash); err != nil {
		t.Fatal(err)
	}

	// Model a prior in-app summary that contains free text.
	if _, err := pg.ExecContext(ctx, `UPDATE ent_notifications SET summary=$1 WHERE outbound_pending`,
		strings.Join([]string{joinPurpose, originalPurpose, rejection, returnReason, secret}, " ")); err != nil {
		t.Fatal(err)
	}
	relayFailure := errors.New("relay failed after writing to outbound queue")
	err = s.ent.ForwardNotifications(ctx, deliveries, func(ctx context.Context, tx *sql.Tx, snap notify.Snapshot) error {
		if err := db.EnqueueNotificationEventTx(ctx, tx, snap); err != nil {
			return err
		}
		return relayFailure
	})
	if !errors.Is(err, relayFailure) {
		t.Fatalf("relay failure = %v", err)
	}
	var pending, enqueued int
	if err := pg.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM ent_notifications WHERE outbound_pending),
		(SELECT count(*) FROM notification_events WHERE id>$1)`, watermark).Scan(&pending, &enqueued); err != nil ||
		pending != deliveries || enqueued != 0 {
		t.Fatalf("failed relay: pending=%d enqueued=%d err=%v", pending, enqueued, err)
	}
	n := newNotifier(s)
	n.forwardEnterpriseNotifications(ctx)
	n.forwardEnterpriseNotifications(ctx)
	if events, deliveries, err := pg.FanOutPendingEvents(ctx, 50); err != nil || events != 8 || deliveries < 8 {
		t.Fatalf("fan-out events=%d deliveries=%d err=%v", events, deliveries, err)
	}

	sent := make(chan struct{})
	go func() {
		defer close(sent)
		n.stepRealtime(ctx, channel, deliveries, "https://artex.example.test")
	}()
	takePayload := func() string {
		t.Helper()
		select {
		case payload := <-payloads:
			return payload
		case <-time.After(5 * time.Second):
			t.Fatal("webhook received no payload")
			return ""
		}
	}
	first := takePayload()
	coreCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := s.ent.SubmitJoin(coreCtx, actor(enterprise.UserDelegate, ""), enterprise.ProjectA, joinPurpose); err != nil {
		t.Fatalf("business transaction while webhook was waiting: %v", err)
	}
	unblock()
	for i := range deliveries {
		payload := first
		if i != 0 {
			payload = takePayload()
		}
		var msg struct {
			Items []struct {
				Event   string   `json:"event_type"`
				Name    string   `json:"name"`
				Summary string   `json:"summary"`
				Finding int64    `json:"finding_id"`
				Assets  []string `json:"assets"`
				Link    string   `json:"detail_url"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(payload), &msg); err != nil || len(msg.Items) != 1 {
			t.Fatalf("webhook payload = %s, %v", payload, err)
		}
		item := msg.Items[0]
		event := strings.TrimPrefix(item.Event, "enterprise_")
		snap, ok := notify.EnterpriseSnapshot(event)
		if !ok || item.Event != snap.Kind || item.Name != snap.Name || item.Summary != snap.Summary ||
			item.Finding != 0 || item.Link != "" || len(item.Assets) != 0 {
			t.Errorf("webhook leaked document data: %s", payload)
		}
		for _, private := range []string{
			joinPurpose, originalPurpose, rejection, returnReason, reportBody, enterprise.DemoSecret,
			sample.ProjectAID, analyst.ID, join.ID, request.ID, report.ID,
		} {
			if strings.Contains(payload, private) {
				t.Errorf("private field %q in webhook payload: %s", private, payload)
			}
		}
		want[event]--
	}
	for event, remaining := range want {
		if remaining != 0 {
			t.Errorf("unexpected delivery count for %s: remaining=%d", event, remaining)
		}
	}
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("notifier did not finish dispatching")
	}
	rows, count, err := pg.ListNotificationDeliveries(ctx, db.NotificationDeliveryFilter{ChannelID: channel.ID, State: db.NotifyStateSent}, 1, deliveries)
	if err != nil || count != deliveries || len(rows) != deliveries {
		t.Fatalf("sent count=%d rows=%d err=%v", count, len(rows), err)
	}
}
