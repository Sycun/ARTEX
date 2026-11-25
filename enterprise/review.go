package enterprise

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	reviewTaskName = "历史门户登录页核查"
	reviewAssetURL = "https://login.portal.northstar.example/"
	reviewAsset    = "login.portal.northstar.example"
)

// ReviewPack 是独立于 DEMO-NS-001 的历史审核样例。它带一份已经签发的脱敏报告，供只读审核直接浏览。
type ReviewPack struct {
	Reused    bool   `json:"reused"`
	ProjectID string `json:"project_id"`
	TaskID    int64  `json:"task_id"`
	FindingID int64  `json:"finding_id"`
	ReportID  string `json:"report_id"`
	VersionID string `json:"version_id"`
	FileHash  string `json:"file_hash"`
	Sample    string `json:"sample"`
}

func (s *Service) EnsureReviewPack(ctx context.Context) (ReviewPack, error) {
	var pack ReviewPack
	pack.Sample = ReviewSampleKey
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('DEMO-RV-001')::bigint)`); err != nil {
			return err
		}
		existing, ok, err := s.savedReview(ctx, tx)
		if err != nil {
			return err
		}
		if ok {
			pack = existing
			pack.Reused = true
			return nil
		}
		projectID, err := s.ensureProject(ctx, tx, ProjectA, "门户整改")
		if err != nil {
			return err
		}
		if err := s.ensureOwner(ctx, tx, projectID, UserOwnerA, UserBiz); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_members (project_id, user_id, role) VALUES ($1,$2,'reader')
			ON CONFLICT DO NOTHING`, projectID, UserReader); err != nil {
			return err
		}
		var explorationID int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO explorations (description, goal, status)
			VALUES ($1,$2,'achieved') RETURNING id`,
			"历史审核样例 DEMO-RV-001。只供查阅，打开不会启动。",
			"核对历史门户登录页的脱敏结论。样例编号 DEMO-RV-001。",
		).Scan(&explorationID); err != nil {
			return err
		}
		completed := time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC)
		var taskID int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO tasks (
				name, description, goal, exploration_id, status, queued, paused,
				completed_at, coverage_enabled, remark)
			VALUES ($1,$2,$3,$4,'done',false,false,$5,false,$6)
			RETURNING id`,
			reviewTaskName,
			"这是历史审核样例 DEMO-RV-001 里已经完成的任务。查看概览、资产、发现或下载报告都不会启动任务，也不会进入队列。",
			"核对历史门户登录页的脱敏结论。样例编号 DEMO-RV-001。",
			explorationID, completed, ReviewSampleKey,
		).Scan(&taskID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_project_tasks (project_id, task_id, sample_key) VALUES ($1,$2,$3)`,
			projectID, taskID, ReviewSampleKey); err != nil {
			return err
		}
		assetID, err := s.ensureReviewAsset(ctx, tx, taskID)
		if err != nil {
			return err
		}
		var findingID int64
		summary := "登录失败时，页面返回了过长的内部错误说明。这是历史审核样例的脱敏摘要，不含口令或原始报错。"
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO findings (task_id, name, severity, summary, evidence, status, asset_ids)
			VALUES ($1,$2,'low',$3,'','confirmed',$4::jsonb) RETURNING id`,
			taskID, "历史审核样例：登录页错误说明", summary, fmt.Sprintf("[%d]", assetID),
		).Scan(&findingID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_project_findings (project_id, finding_id) VALUES ($1,$2)`, projectID, findingID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_asset_obs (id, project_id, asset_name, observation, observed_at)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (project_id, asset_name) DO NOTHING`,
			newID(), projectID, reviewAsset,
			"历史审核样例在 2026-02-20 记录到登录页。观察里没有口令。", completed); err != nil {
			return err
		}
		reportID := newID()
		versionID := newID()
		body := summary + "已签发脱敏版供审核阅读。"
		bodyHash := Hash([]byte(body))
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_reports (id, project_id, title, created_by) VALUES ($1,$2,$3,$4)`,
			reportID, projectID, "历史审核样例：登录页错误说明", UserBiz); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ent_report_sources (report_id, finding_id) VALUES ($1,$2)`, reportID, findingID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ent_finding_report (finding_id, report_id) VALUES ($1,$2)`, findingID, reportID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_versions (id, report_id, number, status, body, body_hash, editors, lineage, frozen_at)
			VALUES ($1,$2,1,'issued',$3,$4,'[]','[]',$5)`,
			versionID, reportID, body, bodyHash, completed); err != nil {
			return err
		}
		file := issuedMarkdown("门户整改", "历史审核样例：登录页错误说明", 1, body)
		fileHash := Hash(file)
		if err := s.audit(ctx, tx, projectID, UserBiz, "review_sample_loaded", "历史审核样例已准备",
			"DEMO-RV-001 已准备：一条已完成任务和一份已签发脱敏报告。它不是本轮 DEMO-NS-001 的签发。",
			"sample", ReviewSampleKey, "", nil); err != nil {
			return err
		}
		saved, err := json.Marshal(ReviewPack{
			ProjectID: projectID, TaskID: taskID, FindingID: findingID,
			ReportID: reportID, VersionID: versionID, FileHash: fileHash, Sample: ReviewSampleKey,
		})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_sample_loads (sample_key, loaded_by, finding_id, result)
			VALUES ($1,$2,$3,$4::jsonb)`, ReviewSampleKey, UserBiz, findingID, string(saved)); err != nil {
			return err
		}
		pack = ReviewPack{
			ProjectID: projectID, TaskID: taskID, FindingID: findingID,
			ReportID: reportID, VersionID: versionID, FileHash: fileHash, Sample: ReviewSampleKey,
		}
		return s.writeIssue(ctx, tx, versionID, UserBiz, fileHash, "门户整改-v1-脱敏报告.md", file, completed)
	})
	return pack, err
}

func (s *Service) savedReview(ctx context.Context, tx *sql.Tx) (ReviewPack, bool, error) {
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT result FROM ent_sample_loads WHERE sample_key=$1`, ReviewSampleKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewPack{}, false, nil
	}
	if err != nil {
		return ReviewPack{}, false, err
	}
	var pack ReviewPack
	if err := json.Unmarshal(raw, &pack); err != nil {
		return ReviewPack{}, false, err
	}
	if pack.TaskID == 0 || pack.VersionID == "" || pack.FileHash == "" {
		return ReviewPack{}, false, fmt.Errorf("review sample record is incomplete")
	}
	pack.Sample = ReviewSampleKey
	return pack, true, nil
}

func (s *Service) ensureReviewAsset(ctx context.Context, tx *sql.Tx, taskID int64) (int64, error) {
	var assetID int64
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM assets WHERE type='service' AND service_type='http' AND url=$1`, reviewAssetURL).Scan(&assetID)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `
			INSERT INTO assets (type, service_type, domain, url, service_name, page_title, task_ids)
			VALUES ('service','http',$1,$2,'门户登录','登录页',ARRAY[$3]::bigint[])
			RETURNING id`, reviewAsset, reviewAssetURL, taskID).Scan(&assetID)
	}
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE assets SET task_ids = (
			SELECT COALESCE(array_agg(DISTINCT x), '{}')
			FROM unnest(task_ids || ARRAY[$2]::bigint[]) AS x
		) WHERE id=$1`, assetID, taskID)
	return assetID, err
}
