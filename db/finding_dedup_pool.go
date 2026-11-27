package db

import (
	"encoding/json"
	"strings"
)

// prefixedNodeCols mirrors node columns with an alias for joins.
const prefixedNodeCols = `n.id, n.kind, n.payload, n.priority, n.state, ` +
	`COALESCE(n.origin,''), COALESCE(n.owner,''), COALESCE(n.blocked_reason,''), ` +
	`COALESCE(n.delete_reason,''), n.created_at`

// FindingsSharingAssets returns finding nodes from other tasks anchored to one
// of assetIDs. Cross-task matches require the current task to share global
// assets, and stay inside the same enterprise project when either task is
// bound to one. Finding cases and report issuance are not consulted.
func (s *ExplorationStore) FindingsSharingAssets(assetIDs []int64, excludeTask int64, limit int) ([]*Node, error) {
	if len(assetIDs) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	ors := make([]string, 0, len(assetIDs))
	args := []any{excludeTask}
	for _, id := range assetIDs {
		args = append(args, "["+itoa(id)+"]")
		ors = append(ors, "f.asset_ids @> $"+itoa(int64(len(args)))+"::jsonb")
	}
	args = append(args, limit)
	q := `SELECT ` + prefixedNodeCols + `, f.id, f.task_id
	      FROM findings f
	      JOIN exploration_nodes n ON n.id = f.node_id
	      WHERE f.task_id <> $1 AND (` + strings.Join(ors, " OR ") + `)
	        AND EXISTS (
	          SELECT 1 FROM tasks current
	          WHERE current.id = $1 AND current.share_global_assets
	        )
	        AND (
	          EXISTS (
	            SELECT 1 FROM ent_project_tasks mine
	            JOIN ent_project_tasks theirs ON theirs.project_id = mine.project_id
	            WHERE mine.task_id = $1 AND theirs.task_id = f.task_id
	          )
	          OR (
	            NOT EXISTS (SELECT 1 FROM ent_project_tasks mine WHERE mine.task_id = $1)
	            AND NOT EXISTS (SELECT 1 FROM ent_project_tasks theirs WHERE theirs.task_id = f.task_id)
	            AND EXISTS (
	              SELECT 1 FROM tasks other
	              WHERE other.id = f.task_id AND other.share_global_assets
	            )
	          )
	        )
	      ORDER BY f.id DESC LIMIT $` + itoa(int64(len(args)))
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		var n Node
		var payload []byte
		var findingID, taskID int64
		if err := rows.Scan(&n.ID, &n.Kind, &payload, &n.Priority, &n.State, &n.Origin, &n.Owner,
			&n.BlockedReason, &n.DeleteReason, &n.CreatedAt, &findingID, &taskID); err != nil {
			return nil, err
		}
		n.Payload = payload
		n.FindingID = findingID
		n.SourceTaskID = taskID
		n.Inherited = true
		out = append(out, &n)
	}
	return out, rows.Err()
}

// FindingDedupMeta is the finding metadata deterministic fingerprint dedup needs.
type FindingDedupMeta struct {
	AssetIDs []int64
	Status   string
}

// FindingDedupMetadata returns asset ids and disposition status keyed by finding id.
func (s *ExplorationStore) FindingDedupMetadata(findingIDs []int64) (map[int64]FindingDedupMeta, error) {
	out := make(map[int64]FindingDedupMeta, len(findingIDs))
	if len(findingIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, 0, len(findingIDs))
	args := make([]any, 0, len(findingIDs))
	seen := make(map[int64]struct{}, len(findingIDs))
	for _, id := range findingIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		args = append(args, id)
		placeholders = append(placeholders, "$"+itoa(int64(len(args))))
	}
	if len(args) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(
		`SELECT id, COALESCE(asset_ids, '[]'::jsonb), COALESCE(status, 'pending') FROM findings WHERE id IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var rawAssets []byte
		var status string
		if err := rows.Scan(&id, &rawAssets, &status); err != nil {
			return nil, err
		}
		var aids []int64
		_ = json.Unmarshal(rawAssets, &aids)
		out[id] = FindingDedupMeta{AssetIDs: aids, Status: status}
	}
	return out, rows.Err()
}
