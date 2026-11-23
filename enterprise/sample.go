package enterprise

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"time"
)

// ProjectCounts 是一组可以展示的数量。基线和当前项目分开，避免后来的签发改写首次装载结果。
type ProjectCounts struct {
	Tasks     int `json:"tasks"`
	Findings  int `json:"findings"`
	Cases     int `json:"cases"`
	Issued    int `json:"issued"`
	Cold      int `json:"cold"`
	Unclaimed int `json:"unclaimed"`
	Running   int `json:"running"`
	Queued    int `json:"queued"`
}

type AdminStep struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Action string `json:"action"`
}

type SampleResult struct {
	Sample         string        `json:"sample"`
	Findings       int           `json:"findings"`
	Cases          int           `json:"cases"`
	Issued         int           `json:"issued"`
	Cold           int           `json:"cold"`
	Unclaimed      int           `json:"unclaimed"`
	Running        int           `json:"running"`
	Queued         int           `json:"queued"`
	FindingID      int64         `json:"finding_id"`
	Reused         bool          `json:"reused"`
	Loaded         bool          `json:"loaded"`
	ProjectAID     string        `json:"project_a_id"`
	ProjectBID     string        `json:"project_b_id"`
	CurrentProject string        `json:"current_project,omitempty"`
	Message        string        `json:"message"`
	Baseline       ProjectCounts `json:"baseline"`
	Current        ProjectCounts `json:"current"`
	Next           []AdminStep   `json:"next"`
}

func (s *Service) ImportSample(ctx context.Context, a Actor) (SampleResult, error) {
	if !a.Platform() {
		return SampleResult{}, fail(404, "not_found", "找不到", nil)
	}
	var ready bool
	if err := s.DB.QueryRowContext(ctx, `SELECT status='approved' FROM ent_init_grants WHERE id='init-demo'`).Scan(&ready); err != nil || !ready {
		return SampleResult{}, fail(409, "grant_missing", "还没有已批准的初始化授权单。授权单要由企业业务负责人批准，这里不能跳过。", nil)
	}
	var result SampleResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('DEMO-NS-001')::bigint)`); err != nil {
			return err
		}
		var existing sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT finding_id FROM ent_sample_loads WHERE sample_key=$1`, SampleKey).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			result.Reused = true
			if existing.Valid {
				result.FindingID = existing.Int64
			}
			return nil
		}
		aID, err := s.ensureProject(ctx, tx, ProjectA, "门户整改")
		if err != nil {
			return err
		}
		bID, err := s.ensureProject(ctx, tx, ProjectB, "季度巡检")
		if err != nil {
			return err
		}
		if err := s.ensureOwner(ctx, tx, aID, UserOwnerA, a.ID); err != nil {
			return err
		}
		if err := s.ensureOwner(ctx, tx, bID, UserOwnerB, a.ID); err != nil {
			return err
		}
		var findingID int64
		err = tx.QueryRowContext(ctx, `
			INSERT INTO findings (name, severity, summary, evidence, status)
			VALUES ($1,'low',$2,'','pending') RETURNING id`,
			"登录页错误信息过于详细",
			"登录失败时，页面返回了过长的内部错误说明。本段不粘贴原文。").Scan(&findingID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ent_project_findings (project_id, finding_id) VALUES ($1,$2)`, aID, findingID); err != nil {
			return err
		}
		shot := pngWithText("password=" + DemoSecret)
		if !bytes.Contains(shot, []byte(DemoSecret)) {
			return fmt.Errorf("demo screenshot lost the fictional credential marker")
		}
		path := filepath.Join(s.Restricted, "demo-ns-001-login.png")
		if err := os.WriteFile(path, shot, 0o600); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_originals (id, project_id, finding_id, filename, sha256, storage_path)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			"orig-demo-ns-001", aID, findingID, "login-error.png", Hash(shot), path); err != nil {
			return err
		}
		obs := []struct {
			project, text string
			at            time.Time
		}{
			{aID, "门户整改在 2026-03-01 记录到一次页面访问。观察里没有口令。", time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)},
			{bID, "季度巡检在 2026-03-18 记录到一次页面访问。", time.Date(2026, 3, 18, 9, 0, 0, 0, time.UTC)},
		}
		for _, item := range obs {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO ent_asset_obs (id, project_id, asset_name, observation, observed_at)
				VALUES ($1,$2,'portal.northstar.example',$3,$4)
				ON CONFLICT (project_id, asset_name) DO NOTHING`, newID(), item.project, item.text, item.at); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_cold_archives (id, sample_key, project_id, task_label, status, restored)
			VALUES ('cold-demo-ns-001',$1,$2,'历史门户核查','restorable',false)`, SampleKey, aID); err != nil {
			return err
		}
		body := []byte("shared-body-demo")
		traffic := []byte("traffic-body-demo")
		if err := os.WriteFile(filepath.Join(s.Restricted, "h-body"), body, 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(s.Restricted, "h-traffic"), traffic, 0o600); err != nil {
			return err
		}
		rows := []struct{ key, hash string }{
			{"U-流量", Hash(traffic)},
			{"U-快照", Hash(body)},
			{"U-文件", Hash(body)},
		}
		for _, row := range rows {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO ent_unclaimed (id, sample_key, object_key, body_hash, status)
				VALUES ($1,$2,$3,$4,'registered')`, newID(), SampleKey, row.key, row.hash); err != nil {
				return err
			}
		}
		baseline, err := json.Marshal(map[string]any{"baseline": ProjectCounts{
			Findings: 1, Cases: 0, Issued: 0, Cold: 1, Unclaimed: 3,
		}})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_sample_loads (sample_key, loaded_by, finding_id, result)
			VALUES ($1,$2,$3,$4::jsonb)`, SampleKey, a.ID, findingID, string(baseline)); err != nil {
			return err
		}
		result.FindingID = findingID
		return s.audit(ctx, tx, "", a.ID, "sample_imported", "导入演示样例", "导入 DEMO-NS-001", "sample", SampleKey, "", nil)
	})
	if err != nil {
		return SampleResult{}, err
	}
	filled, err := s.sampleView(ctx)
	if err != nil {
		return SampleResult{}, err
	}
	filled.Reused = result.Reused
	filled.FindingID = result.FindingID
	if filled.FindingID == 0 {
		_ = s.DB.QueryRowContext(ctx, `SELECT finding_id FROM ent_sample_loads WHERE sample_key=$1`, SampleKey).Scan(&filled.FindingID)
	}
	if filled.Reused {
		filled.Message = "DEMO-NS-001 已经装载。没有新增发现，也没有清掉报告、审批记录或历史审核样例。"
	} else {
		filled.Message = "已导入 DEMO-NS-001。首次装载基线里，热库已签发报告是 0。当前项目计数另列在下面，不会改写这份基线。"
	}
	return filled, nil
}

func (s *Service) sampleView(ctx context.Context) (SampleResult, error) {
	var out SampleResult
	out.Sample = SampleKey
	out.Next = adminNext()
	base, loaded, err := s.readBaseline(ctx)
	if err != nil {
		return SampleResult{}, err
	}
	out.Loaded = loaded
	out.Baseline = base
	if loaded {
		out.Findings, out.Cases, out.Issued, out.Cold, out.Unclaimed = base.Findings, base.Cases, base.Issued, base.Cold, base.Unclaimed
	}
	var projectA, projectB, projectName string
	_ = s.DB.QueryRowContext(ctx, `SELECT id, name FROM ent_projects WHERE code=$1`, ProjectA).Scan(&projectA, &projectName)
	_ = s.DB.QueryRowContext(ctx, `SELECT id FROM ent_projects WHERE code=$1`, ProjectB).Scan(&projectB)
	out.ProjectAID, out.ProjectBID = projectA, projectB
	if projectName != "" {
		out.CurrentProject = projectName + "（" + ProjectA + "）"
	}
	current, err := s.projectCounts(ctx, projectA)
	if err != nil {
		return SampleResult{}, err
	}
	running, queued, err := s.Runtime(ctx)
	if err != nil {
		return SampleResult{}, err
	}
	current.Running, current.Queued = running, queued
	out.Current = current
	out.Running, out.Queued = running, queued
	return out, nil
}

func (s *Service) readBaseline(ctx context.Context) (ProjectCounts, bool, error) {
	var raw []byte
	err := s.DB.QueryRowContext(ctx, `SELECT result FROM ent_sample_loads WHERE sample_key=$1`, SampleKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectCounts{}, false, nil
	}
	if err != nil {
		return ProjectCounts{}, false, err
	}
	var wrap struct {
		Baseline *ProjectCounts `json:"baseline"`
	}
	if json.Unmarshal(raw, &wrap) == nil && wrap.Baseline != nil {
		return *wrap.Baseline, true, nil
	}
	frozen := ProjectCounts{Findings: 1, Cases: 0, Issued: 0, Cold: 1, Unclaimed: 3}
	blob, err := json.Marshal(map[string]any{"baseline": frozen})
	if err != nil {
		return ProjectCounts{}, false, err
	}
	_, err = s.DB.ExecContext(ctx, `
		UPDATE ent_sample_loads
		SET result = COALESCE(result, '{}'::jsonb) || $2::jsonb
		WHERE sample_key=$1 AND NOT (COALESCE(result, '{}'::jsonb) ? 'baseline')`, SampleKey, string(blob))
	return frozen, true, err
}

func (s *Service) projectCounts(ctx context.Context, projectID string) (ProjectCounts, error) {
	var out ProjectCounts
	if projectID == "" {
		return out, nil
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM ent_project_tasks WHERE project_id=$1`, projectID).Scan(&out.Tasks); err != nil {
		return ProjectCounts{}, err
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM ent_project_findings WHERE project_id=$1`, projectID).Scan(&out.Findings); err != nil {
		return ProjectCounts{}, err
	}
	if err := s.DB.QueryRowContext(ctx, `
		SELECT count(*) FROM finding_cases c
		WHERE EXISTS (
			SELECT 1 FROM finding_case_members m
			JOIN ent_project_findings p ON p.finding_id=m.finding_id
			WHERE m.case_id=c.id AND p.project_id=$1)`, projectID).Scan(&out.Cases); err != nil {
		return ProjectCounts{}, err
	}
	if err := s.DB.QueryRowContext(ctx, `
		SELECT count(*) FROM ent_versions v JOIN ent_reports r ON r.id=v.report_id
		WHERE r.project_id=$1 AND v.status='issued'`, projectID).Scan(&out.Issued); err != nil {
		return ProjectCounts{}, err
	}
	if err := s.DB.QueryRowContext(ctx, `
		SELECT count(*) FROM ent_cold_archives WHERE project_id=$1 AND NOT restored`, projectID).Scan(&out.Cold); err != nil {
		return ProjectCounts{}, err
	}
	if err := s.DB.QueryRowContext(ctx, `
		SELECT count(*) FROM ent_unclaimed u
		WHERE EXISTS (
			SELECT 1 FROM ent_sample_loads l WHERE l.sample_key=u.sample_key AND l.sample_key=$1)`, SampleKey).Scan(&out.Unclaimed); err != nil {
		return ProjectCounts{}, err
	}
	return out, nil
}

func adminNext() []AdminStep {
	return []AdminStep{
		{Title: "返回企业首页", Detail: "首页说明平台管理和项目工作区各自做什么。导入样例不会把项目任务授给平台管理员。", Action: "home"},
		{Title: "查看样例状态", Detail: "首次装载基线保持 DEMO-NS-001 导入当时的数字。当前计数是项目里现在的任务、发现和已签发报告。再次导入不会清掉报告或审批。", Action: "status"},
		{Title: "切换到项目审核账号", Detail: "退出后在企业登录页选择梁阅（demo-reader-a）。这是一次正常登录，不会在当前会话里改角色。", Action: "switch-reader"},
	}
}

func (s *Service) SampleStatus(ctx context.Context, a Actor) (SampleResult, error) {
	if !a.Platform() {
		return SampleResult{}, fail(404, "not_found", "找不到", nil)
	}
	out, err := s.sampleView(ctx)
	if err != nil {
		return SampleResult{}, err
	}
	var id sql.NullInt64
	err = s.DB.QueryRowContext(ctx, `SELECT finding_id FROM ent_sample_loads WHERE sample_key=$1`, SampleKey).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		out.Message = "还没有导入 DEMO-NS-001。初始化授权单已批准后可以导入。历史审核样例如果已经准备好，会算在当前项目计数里，不会写进这份基线。"
		return out, nil
	}
	if err != nil {
		return SampleResult{}, err
	}
	if id.Valid {
		out.FindingID = id.Int64
	}
	out.Reused = true
	out.Message = "DEMO-NS-001 已装载。下面的基线是首次装载时记下的，当前计数反映项目里现在的记录。"
	return out, nil
}

func (s *Service) ensureProject(ctx context.Context, tx *sql.Tx, code, name string) (string, error) {
	id := newID()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ent_projects (id, code, name, status) VALUES ($1,$2,$3,'active')
		ON CONFLICT (code) DO NOTHING`, id, code, name); err != nil {
		return "", err
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM ent_projects WHERE code=$1`, code).Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Service) ensureOwner(ctx context.Context, tx *sql.Tx, projectID, userID, grantedBy string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ent_members (project_id, user_id, role) VALUES ($1,$2,'owner')
		ON CONFLICT DO NOTHING`, projectID, userID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO ent_duties (id, project_id, user_id, duty, granted_by, grant_ref)
		VALUES ($1,$2,$3,'approve',$4,'init-demo')
		ON CONFLICT (project_id, user_id, duty) DO NOTHING`, newID(), projectID, userID, grantedBy)
	return err
}

func pngWithText(text string) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range img.Pix {
		img.Pix[i] = 180
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	raw := buf.Bytes()
	if len(raw) < 12 {
		return append(raw, []byte(text)...)
	}
	data := append([]byte("Comment\x00"), []byte(text)...)
	payload := append([]byte("tEXt"), data...)
	var chunk bytes.Buffer
	_ = binary.Write(&chunk, binary.BigEndian, uint32(len(data)))
	chunk.Write(payload)
	_ = binary.Write(&chunk, binary.BigEndian, crc32.ChecksumIEEE(payload))
	out := append([]byte{}, raw[:len(raw)-12]...)
	out = append(out, chunk.Bytes()...)
	out = append(out, raw[len(raw)-12:]...)
	return out
}
