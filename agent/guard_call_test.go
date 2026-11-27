package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

var (
	testSecretsOnce sync.Once
	testSecretsDir  string
)

func ensureTestSecrets(t *testing.T, d *db.DB) {
	t.Helper()
	if os.Getenv("ARTEX_SECRET_KEY") == "" {
		t.Setenv("ARTEX_SECRET_KEY", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	}
	testSecretsOnce.Do(func() {
		dir := filepath.Join(os.TempDir(), "artex-agent-test-secrets")
		_ = os.MkdirAll(dir, 0o700)
		testSecretsDir = dir
	})
	_ = testSecretsDir
}

func nodeState(t *testing.T, s *db.ExplorationStore, id int64) string {
	t.Helper()
	n, err := s.GetNode(id)
	if err != nil || n == nil {
		t.Fatalf("node %d: %v", id, err)
	}
	return n.State
}

func seedFindingAsset(t *testing.T, d *db.DB, task *db.Task, rawURL string) int64 {
	t.Helper()
	id, err := d.Assets().UpsertHTTPService(db.UpsertHTTPServiceReq{URL: rawURL, TaskID: task.ID})
	if err != nil {
		t.Fatalf("seed finding asset: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM assets WHERE id=$1`, id) })
	return id
}

func seedProvenFinding(t *testing.T, d *db.DB, task *db.Task, trafficID, rawURL string, assetIDs []int64) *db.RecordedFinding {
	t.Helper()
	ensureTestSecrets(t, d)
	snap := db.TrafficEvidenceSnapshot{
		SourceTrafficID: trafficID, URL: rawURL, Method: "GET", Status: 200,
		ReqHash: "req-" + trafficID, RespHash: "resp-" + trafficID,
	}
	snap.ID = db.TrafficSnapshotID(snap)
	var rec *db.RecordedFinding
	err := d.WithEvidenceTx(context.Background(), func(tx *sql.Tx) error {
		var e error
		rec, e = db.RecordFindingTx(context.Background(), tx, db.RecordFindingInput{
			TaskID: task.ID, ExplorationID: task.ExplorationID, VulnClass: "sqli", Name: "Users dump",
			Severity: "high", Summary: "user table dumped with traffic proof", Worker: "worker-1", AssetIDs: assetIDs,
		}, []db.PreparedTrafficEvidence{{
			Ref: db.TrafficRef{TrafficID: trafficID, Role: "proof", Note: "verified capture"}, Snapshot: snap,
		}})
		return e
	})
	if err != nil {
		t.Fatalf("seed proven finding: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM finding_traffic_bindings WHERE finding_id=$1`, rec.FindingID)
		_, _ = d.Exec(`DELETE FROM traffic_evidence_snapshots WHERE id=$1`, snap.ID)
	})
	return rec
}

func guardTaskStore(t *testing.T) *db.ExplorationStore {
	t.Helper()
	d := testDB(t)
	t.Cleanup(func() { _ = d.Close() })
	task, err := d.CreateTaskWithOptions("agentic guards", "exercise the goal and hint guards", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	return d.Exploration(task.ExplorationID)
}

func addGuardNode(t *testing.T, s *db.ExplorationStore, kind string, payload map[string]any, state string) int64 {
	t.Helper()
	id, err := s.AddNode(kind, payload, 0, state, "worker-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func callGuardTool(t *testing.T, tool actool.CoreTool, input string) actool.Result {
	t.Helper()
	res, err := tool.Call(context.Background(), json.RawMessage(input), nil)
	if err != nil {
		t.Fatalf("%s: transport error: %v", tool.Name(), err)
	}
	return res
}
