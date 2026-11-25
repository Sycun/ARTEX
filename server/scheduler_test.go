package server

import (
	"context"
	"database/sql"
	"net/url"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/jackc/pgx/v5"
)

func dsnWithAppName(t *testing.T, dsn, name string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("application_name", name)
	u.RawQuery = q.Encode()
	return u.String()
}

func TestSchedulerLeaderElection(t *testing.T) {
	dsn, pg := controlPlaneTestDB(t)
	tap, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeNotifyListener(tap) })
	if _, err := tap.Exec(t.Context(), `LISTEN artex_test_scheduler_fire`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`
CREATE FUNCTION artex_test_scheduler_fire() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_notify('artex_test_scheduler_fire', current_setting('application_name'));
  RETURN NEW;
END;
$$;
CREATE TRIGGER artex_test_scheduler_fire AFTER UPDATE OF last_fire ON agent_triggers
FOR EACH ROW WHEN (NEW.agent_key = 'artex-test-leader' AND NEW.last_fire IS NOT NULL)
EXECUTE FUNCTION artex_test_scheduler_fire()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pg.Exec(`DROP TRIGGER artex_test_scheduler_fire ON agent_triggers;
DROP FUNCTION artex_test_scheduler_fire()`); err != nil {
			t.Error(err)
		}
	})

	for _, mode := range []string{"shutdown", "session-loss"} {
		t.Run(mode, func(t *testing.T) {
			trigger, err := pg.CreateTrigger(&db.AgentTrigger{AgentKey: "artex-test-leader", Enabled: true, IntervalSec: 3600})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := pg.DeleteTrigger(trigger.ID); err != nil {
					t.Error(err)
				}
			})
			expectFire := func(name string) {
				t.Helper()
				ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
				defer cancel()
				n, err := tap.WaitForNotification(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if n.Payload != name {
					t.Fatalf("trigger fired on %q, want %q", n.Payload, name)
				}
			}
			makeDue := func() {
				t.Helper()
				if _, err := pg.Exec(`UPDATE agent_triggers SET last_fire=NULL WHERE id=$1`, trigger.ID); err != nil {
					t.Fatal(err)
				}
			}
			start := func(name string, tick time.Duration) func() {
				t.Helper()
				pool, err := sql.Open("pgx", dsnWithAppName(t, dsn, name))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := pool.Close(); err != nil {
						t.Error(err)
					}
				})
				store := &db.DB{DB: pool}
				m := &Manager{pg: store}
				s := &Server{m: m, engine: NewEngine(m)}
				sc := newScheduler(s)
				sc.tick = tick
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() {
					defer close(done)
					sc.Run(ctx)
				}()
				stop := func() {
					cancel()
					<-done
				}
				t.Cleanup(stop)
				return stop
			}

			const leader = "artex-test-leader-a"
			const follower = "artex-test-leader-b"
			stopLeader := start(leader, 500*time.Millisecond)
			expectFire(leader)
			start(follower, 30*time.Millisecond)
			makeDue()
			expectFire(leader) // The follower has multiple ticks while the trigger is due.

			var pid int
			if err := pg.QueryRow(`
SELECT l.pid FROM pg_locks l JOIN pg_stat_activity a USING (pid)
WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 1
  AND l.classid = ($1::bigint >> 32)::oid AND l.objid = ($1::bigint & 4294967295)::oid
  AND a.datname = current_database() AND a.application_name = $2`, schedulerLockKey, leader).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			if mode == "shutdown" {
				stopLeader()
			} else {
				var terminated bool
				err := pg.QueryRow(`SELECT pg_terminate_backend($1, 1000)`, pid).Scan(&terminated)
				if err != nil || !terminated {
					t.Fatalf("terminate leader session %d: %t %v", pid, terminated, err)
				}
			}
			makeDue()
			expectFire(follower)
		})
	}
}
