package server

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/jackc/pgx/v5"
)

func TestMemoryBus(t *testing.T) {
	bus, err := configuredEventBus(t.Context(), "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bus.(*MemoryBus); !ok {
		t.Fatalf("default event bus = %T", bus)
	}
	b := &Broadcaster{bus: bus}
	fast, stopFast := b.Subscribe("1")
	defer stopFast()
	slow, stopSlow := b.Subscribe("1")
	defer stopSlow()
	other, stopOther := b.Subscribe("2")
	defer stopOther()
	control, stopControl := bus.SubscribeControl()
	defer stopControl()

	for id := int64(1); id <= 300; id++ {
		a := db.Activity{ID: id, Detail: "unchanged"}
		b.Publish("1", a)
		if got := receiveBusEvent(t, fast); !reflect.DeepEqual(got, a) {
			t.Fatalf("got %+v, want %+v", got, a)
		}
	}
	if len(slow) != 256 || len(other) != 0 {
		t.Fatalf("slow buffer = %d, unrelated task buffer = %d", len(slow), len(other))
	}
	event := ControlEvent{Kind: ControlTaskPause, TaskID: "1"}
	if err := bus.PublishControl(event); err != nil {
		t.Fatal(err)
	}
	if got := receiveBusEvent(t, control); got != event {
		t.Fatalf("control = %+v, want %+v", got, event)
	}
	stopFast()
	stopFast()
	b.Publish("1", db.Activity{ID: 301})
	if _, open := <-fast; open {
		t.Fatal("unsubscribe left the channel open")
	}
}

func receiveBusEvent[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v, ok := <-ch:
		if !ok {
			t.Fatal("subscription closed")
		}
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for bus event")
		var zero T
		return zero
	}
}

func controlPlaneTestDB(t *testing.T) (string, *db.DB) {
	t.Helper()
	if os.Getenv("ARTEX_PG_DSN") == "" {
		t.Skip("requires isolated ARTEX_PG_DSN")
	}
	dsn, _, err := db.DSN()
	if err != nil {
		t.Fatal(err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pg.Close(); err != nil {
			t.Error(err)
		}
	})
	return dsn, pg
}

func startTestPGNotifyBus(t *testing.T, dsn string, pg *db.DB) *PGNotifyBus {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	b, err := NewPGNotifyBus(ctx, dsn, pg)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		<-b.done
	})
	return b
}

const pgBusChildTaskEnv = "ARTEX_TEST_PG_BUS_CHILD_TASK"

func TestPGNotifyBusAcrossProcesses(t *testing.T) {
	dsn, pg := controlPlaneTestDB(t)
	sender := startTestPGNotifyBus(t, dsn, pg)
	task, err := pg.CreateTask("event bus fixture", "", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pg.DeleteTask(task.ID); err != nil {
			t.Error(err)
		}
	})
	taskID := fmt.Sprint(task.ID)
	control, stopControl := sender.SubscribeControl()
	defer stopControl()

	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPGNotifyBusProcessHelper$")
	child.Env = append(os.Environ(), pgBusChildTaskEnv+"="+taskID)
	child.Stderr = os.Stderr
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if child.ProcessState == nil {
			cancel()
			if err := child.Wait(); err != nil && !t.Failed() {
				t.Error(err)
			}
		}
	}()
	output := bufio.NewScanner(stdout)
	if !output.Scan() || output.Text() != "pg bus ready" {
		t.Fatalf("receiver did not start: %q (%v)", output.Text(), output.Err())
	}

	store := pg.Exploration(task.ExplorationID)
	tap, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer closeNotifyListener(tap)
	if _, err := tap.Exec(ctx, "LISTEN "+pgActivityChannel); err != nil {
		t.Fatal(err)
	}
	for _, want := range []db.Activity{
		{Kind: "text", Summary: "small", Detail: "persisted detail"},
		{Kind: "text", Summary: strings.Repeat("活动", 2000), Metadata: json.RawMessage(`{"value":"oversized"}`)},
	} {
		want.ID, err = store.AppendActivity(want)
		if err != nil {
			t.Fatal(err)
		}
		if err := sender.PublishActivity(taskID, want); err != nil {
			t.Fatal(err)
		}
		n, err := tap.WaitForNotification(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(n.Payload) > pgNotifyMaxBytes || strings.Contains(n.Payload, want.Summary) {
			t.Fatalf("expected a compact reference, got %d bytes", len(n.Payload))
		}
		if !output.Scan() || output.Text() != activityReceipt(want) {
			t.Fatalf("receiver did not recover activity %d: %q (%v)", want.ID, output.Text(), output.Err())
		}
	}
	cancelEvent := ControlEvent{Kind: ControlTaskCancel, TaskID: taskID}
	if err := sender.PublishControl(cancelEvent); err != nil {
		t.Fatal(err)
	}
	if got := receiveBusEvent(t, control); got != cancelEvent {
		t.Fatalf("control got %+v, want %+v", got, cancelEvent)
	}
	if err := sender.PublishControl(ControlEvent{Kind: ControlTaskPause, TaskID: taskID}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []ControlEventKind{ControlTaskPause, ControlTaskResume} {
		if got := receiveBusEvent(t, control); got != (ControlEvent{Kind: kind, TaskID: taskID}) {
			t.Fatalf("control got %+v, want %s for %s", got, kind, taskID)
		}
	}
	for output.Scan() {
		t.Log(output.Text())
	}
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	if len(control) != 0 {
		t.Fatal("received a duplicate local control notification")
	}
}

func TestPGNotifyBusSSEForTaskCreatedOnAnotherReplica(t *testing.T) {
	dsn, pg := controlPlaneTestDB(t)
	sender := startTestPGNotifyBus(t, dsn, pg)
	receiver := startTestPGNotifyBus(t, dsn, pg)
	manager := &Manager{pg: pg}
	s := &Server{m: manager, engine: newEngine(manager, &Broadcaster{bus: receiver})}
	task, err := pg.CreateTask("remote SSE fixture", "", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pg.DeleteTask(task.ID); err != nil {
			t.Error(err)
		}
	})

	httpServer := httptest.NewServer(http.HandlerFunc(s.streamActivity))
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	taskID := fmt.Sprint(task.ID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"?task="+taskID, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpServer.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("SSE response = %s, headers = %v", resp.Status, resp.Header)
	}

	event := db.Activity{Kind: "text", Summary: "received on the other replica", Detail: "same detail"}
	event.ID, err = pg.Exploration(task.ExplorationID).AppendActivity(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.PublishActivity(taskID, event); err != nil {
		t.Fatal(err)
	}
	body := bufio.NewScanner(resp.Body)
	if !body.Scan() || body.Text() != fmt.Sprintf("id: %d", event.ID) {
		t.Fatalf("SSE id line = %q, err = %v", body.Text(), body.Err())
	}
	var got ActivityDTO
	if !body.Scan() {
		t.Fatalf("missing SSE data: %v", body.Err())
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(body.Text(), "data: ")), &got); err != nil {
		t.Fatal(err)
	}
	if got.Summary != event.Summary || got.Detail != event.Detail {
		t.Fatalf("SSE activity = %+v", got)
	}
}

func TestPGNotifyBusProcessHelper(t *testing.T) {
	taskID := os.Getenv(pgBusChildTaskEnv)
	if taskID == "" {
		t.Skip("subprocess helper")
	}
	dsn, pg := controlPlaneTestDB(t)
	b := startTestPGNotifyBus(t, dsn, pg)
	activity, stopActivity := b.SubscribeActivity(taskID)
	defer stopActivity()
	control, stopControl := b.SubscribeControl()
	defer stopControl()
	fmt.Println("pg bus ready")
	for range 2 {
		fmt.Println(activityReceipt(receiveBusEvent(t, activity)))
	}
	if got := receiveBusEvent(t, control); got != (ControlEvent{Kind: ControlTaskCancel, TaskID: taskID}) {
		t.Fatalf("unexpected control: %+v", got)
	}
	if got := receiveBusEvent(t, control); got != (ControlEvent{Kind: ControlTaskPause, TaskID: taskID}) {
		t.Fatalf("unexpected control: %+v", got)
	}
	if err := b.PublishControl(ControlEvent{Kind: ControlTaskResume, TaskID: taskID}); err != nil {
		t.Fatal(err)
	}
}

func activityReceipt(a db.Activity) string {
	return fmt.Sprintf("activity %d %x %x", a.ID, sha256.Sum256([]byte(a.Summary)), sha256.Sum256([]byte(a.Detail)))
}

func TestPGNotifyBusReconnect(t *testing.T) {
	dsn, pg := controlPlaneTestDB(t)
	sender := startTestPGNotifyBus(t, dsn, pg)
	receiver := startTestPGNotifyBus(t, dsnWithAppName(t, dsn, "artex-test-bus-receiver"), pg)
	activity, stop := receiver.SubscribeActivity("1")
	defer stop()
	control, stop := receiver.SubscribeControl()
	defer stop()

	var terminated bool
	err := pg.QueryRow(`SELECT pg_terminate_backend(pid, 1000) FROM pg_stat_activity
WHERE application_name = 'artex-test-bus-receiver' AND datname = current_database()`).Scan(&terminated)
	if err != nil || !terminated {
		t.Fatalf("terminate LISTEN session: %t %v", terminated, err)
	}

	event := ControlEvent{Kind: ControlTaskDelete, TaskID: "1"}
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for reconnected := false; !reconnected; {
		if err := sender.PublishControl(event); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-control:
			if got != event {
				t.Fatalf("after reconnect: %+v, want %+v", got, event)
			}
			reconnected = true
		case <-tick.C:
		case <-deadline:
			t.Fatal("LISTEN session did not reconnect")
		}
	}

	nodeID := int64(123)
	for _, summary := range []string{"small transient", strings.Repeat("活动", 3000)} {
		if err := sender.PublishActivity("1", db.Activity{NodeID: &nodeID, Summary: summary}); err != nil {
			t.Fatal(err)
		}
		want := summary
		if len(summary) > pgNotifyMaxBytes {
			want = ""
		}
		got := receiveBusEvent(t, activity)
		if got.Summary != want || got.NodeID == nil || *got.NodeID != nodeID {
			t.Fatalf("unexpected transient activity after reconnect: summary bytes=%d, node=%v", len(got.Summary), got.NodeID)
		}
	}
}
