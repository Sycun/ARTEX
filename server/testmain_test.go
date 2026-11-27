package server

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func skipWithoutDSN(t *testing.T) {
	t.Helper()
	if _, _, err := db.DSN(); err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
}

func stopTestServer(t *testing.T, s *Server, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	if s == nil || s.m == nil || s.engine == nil {
		return
	}
	for _, task := range s.m.List() {
		s.engine.BeginDelete(task.ID)
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := s.waitTaskQuiescent(waitCtx, task.ID); err != nil {
			t.Errorf("task %s still running at teardown: %v", task.ID, err)
		}
		waitCancel()
		s.engine.StopTask(task.ID)
	}
	s.archiveWG.Wait()
	if s.side != nil && s.side.done != nil {
		select {
		case <-s.side.done:
		case <-time.After(2 * time.Second):
		}
	}
}

// TestMain acquires a PostgreSQL advisory lock (7337741002) for the entire
// server test suite so cross-package DELETE cleanup races with db/agent
// packages are avoided when running `go test ./...`.
func TestMain(m *testing.M) {
	if os.Getenv(pgBusChildTaskEnv) != "" {
		os.Exit(m.Run()) // The parent process holds the suite lock.
	}
	dsn, _, err := db.DSN()
	if err != nil {
		os.Exit(m.Run())
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil || conn.Ping() != nil {
		os.Exit(m.Run())
	}
	defer conn.Close()
	if _, err := conn.Exec(`SELECT pg_advisory_lock(7337741002)`); err != nil {
		os.Exit(m.Run())
	}
	defer conn.Exec(`SELECT pg_advisory_unlock(7337741002)`) //nolint:errcheck
	// Packages that ran earlier in this database may have left non-terminal
	// tasks. Startup resumes every one of them into this process's data
	// directory, so settle them before the suite starts.
	_, _ = conn.Exec(`UPDATE tasks SET status='done' WHERE status NOT IN ('done','failed','timeout')`)
	os.Exit(m.Run())
}
