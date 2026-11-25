package enterprise

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Autumn-27/artex/storage"
	"github.com/google/uuid"
)

const (
	IdleTimeout          = 30 * time.Minute
	AbsoluteTimeout      = 8 * time.Hour
	sessionTouchInterval = 2 * time.Minute

	UserAdmin    = "usr-admin"
	UserOwnerA   = "usr-owner-a"
	UserAnalyst  = "usr-analyst"
	UserReviewer = "usr-reviewer"
	UserAuditor  = "usr-auditor"
	UserOwnerB   = "usr-owner-b"
	UserMemberB  = "usr-member-b"
	UserDisabled = "usr-disabled"
	UserDelegate = "usr-delegate"
	UserBiz      = "usr-biz"
	UserReader   = "usr-reader-a"

	SampleKey       = "DEMO-NS-001"
	ReviewSampleKey = "DEMO-RV-001"
	ProjectA        = "NS-A"
	ProjectB        = "NS-B"
	ReaderLogin     = "demo-reader-a"
	ReaderName      = "梁阅"
)

// Fail 是可以展示给用户的业务拒绝。
type Fail struct {
	Status  int
	Code    string
	Message string
	Fields  map[string]any
}

func (f *Fail) Error() string { return f.Message }

func fail(status int, code, msg string, fields map[string]any) *Fail {
	return &Fail{Status: status, Code: code, Message: msg, Fields: fields}
}

// Service 执行企业版身份、项目、报告和审计规则。
type Service struct {
	DB         *sql.DB
	DataDir    string
	Restricted string
	Clock      func() time.Time
	blobs      storage.StreamBlobStore
	localBlobs *storage.LocalFSStore
}

func New(sqlDB *sql.DB, dataDir string) (*Service, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	restricted := filepath.Join(filepath.Dir(abs), "restricted")
	if err := checkRestrictedDir(abs, restricted); err != nil {
		return nil, err
	}
	local, err := storage.NewLocalFSStore(restricted)
	if err != nil {
		return nil, err
	}
	var blobs storage.StreamBlobStore = local
	cfg := storage.S3Config{
		Endpoint:  os.Getenv("ARTEX_S3_ENDPOINT"),
		Bucket:    os.Getenv("ARTEX_S3_BUCKET"),
		Region:    os.Getenv("ARTEX_S3_REGION"),
		AccessKey: os.Getenv("ARTEX_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("ARTEX_S3_SECRET_KEY"),
	}
	if cfg != (storage.S3Config{}) {
		blobs, err = storage.NewS3Store(cfg)
		if err != nil {
			return nil, err
		}
	}
	svc := &Service{DB: sqlDB, DataDir: abs, Restricted: restricted, blobs: blobs, localBlobs: local}
	if err := svc.migrate(context.Background()); err != nil {
		return nil, err
	}
	return svc, nil
}

func (s *Service) BlobStore() storage.StreamBlobStore {
	return s.blobs
}

func checkRestrictedDir(dataDir, restricted string) error {
	if dataDir == restricted {
		return fmt.Errorf("restricted store %s is inside data directory %s", restricted, dataDir)
	}
	realData, err := filepath.EvalSymlinks(dataDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(restricted))
	if err != nil {
		return err
	}
	realRestricted := filepath.Join(parent, filepath.Base(restricted))
	rel, err := filepath.Rel(realData, realRestricted)
	if err != nil {
		return err
	}
	if filepath.IsLocal(rel) {
		return fmt.Errorf("restricted store %s is inside data directory %s", restricted, dataDir)
	}
	return nil
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

func (s *Service) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

type User struct {
	ID               string
	Name             string
	DirectoryStatus  string
	Employment       string
	PlatformRole     string
	ExternalID       string
	DirectoryVersion int64
}

func (u User) Gate() string {
	switch {
	case u.DirectoryStatus == "disabled" || (u.DirectoryStatus == "verified" && u.Employment != "active"):
		return "disabled"
	case u.DirectoryStatus != "verified" || u.Employment != "active":
		return "pending_bind"
	default:
		return "ready"
	}
}

func (u User) Platform() bool { return u.PlatformRole == "admin" }

type Actor struct {
	User
	SessionID      string
	SessionProject string
}

func (s *Service) Seed(ctx context.Context) error {
	users := []User{
		{ID: UserAdmin, Name: "林衡", DirectoryStatus: "verified", Employment: "active", PlatformRole: "admin", ExternalID: "demo-admin"},
		{ID: UserReader, Name: ReaderName, DirectoryStatus: "verified", Employment: "active", ExternalID: ReaderLogin},
		{ID: UserBiz, Name: "顾清", DirectoryStatus: "verified", Employment: "active", ExternalID: "demo-biz"},
		{ID: UserOwnerA, Name: "沈昭", DirectoryStatus: "verified", Employment: "active", ExternalID: "demo-owner-a"},
		{ID: UserAnalyst, Name: "周宁", DirectoryStatus: "verified", Employment: "active", ExternalID: "demo-analyst"},
		{ID: UserReviewer, Name: "陈溯", DirectoryStatus: "verified", Employment: "active", ExternalID: "demo-reviewer"},
		{ID: UserAuditor, Name: "许衡", DirectoryStatus: "verified", Employment: "active", ExternalID: "demo-auditor"},
		{ID: UserOwnerB, Name: "江澄", DirectoryStatus: "verified", Employment: "active", ExternalID: "demo-owner-b"},
		{ID: UserMemberB, Name: "苏晚", DirectoryStatus: "verified", Employment: "active", ExternalID: "demo-member-b"},
		{ID: UserDelegate, Name: "裴独立", DirectoryStatus: "verified", Employment: "active", ExternalID: "demo-delegate"},
		{ID: UserDisabled, Name: "停用示例", DirectoryStatus: "disabled", Employment: "inactive", ExternalID: "demo-disabled", DirectoryVersion: 2},
	}
	for _, u := range users {
		if _, err := s.DB.ExecContext(ctx, `
			INSERT INTO ent_users (id, display_name, directory_status, employment, platform_role, external_id, directory_version)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (id) DO UPDATE SET
				display_name=EXCLUDED.display_name,
				directory_status=EXCLUDED.directory_status,
				employment=EXCLUDED.employment,
				platform_role=EXCLUDED.platform_role,
				external_id=EXCLUDED.external_id,
				directory_version=EXCLUDED.directory_version`,
			u.ID, u.Name, u.DirectoryStatus, u.Employment, u.PlatformRole, u.ExternalID, u.DirectoryVersion); err != nil {
			return err
		}
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO ent_init_grants (id, status, approved_by, note)
		VALUES ('init-demo','approved',$1,'企业业务负责人顾清已批准初始化授权，可以创建项目并导入演示样例。')
		ON CONFLICT (id) DO NOTHING`, UserBiz)
	return err
}

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var ext sql.NullString
	err := row.Scan(&u.ID, &u.Name, &u.DirectoryStatus, &u.Employment, &u.PlatformRole, &ext, &u.DirectoryVersion)
	if ext.Valid {
		u.ExternalID = ext.String
	}
	return u, err
}

const userCols = `id, display_name, directory_status, employment, platform_role, external_id, directory_version`

func (s *Service) userByID(ctx context.Context, id string) (User, error) {
	u, err := scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM ent_users WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fail(404, "not_found", "找不到这个人", nil)
	}
	return u, err
}

// BindLogin 按约定 A 把 OIDC sub 收成一个内部用户。无法匹配时保持待绑定，停用账号不另开在职身份。
func (s *Service) BindLogin(ctx context.Context, issuer, subject string) (User, error) {
	var userID string
	err := s.DB.QueryRowContext(ctx, `SELECT user_id FROM ent_identities WHERE issuer=$1 AND subject=$2`, issuer, subject).Scan(&userID)
	if err == nil {
		return s.userByID(ctx, userID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return User{}, err
	}
	var u User
	u, err = scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM ent_users WHERE external_id=$1`, subject))
	if errors.Is(err, sql.ErrNoRows) {
		u = User{
			ID:              uuid.NewString(),
			Name:            "未核验用户",
			DirectoryStatus: "pending_bind",
			Employment:      "inactive",
		}
		if _, err := s.DB.ExecContext(ctx, `
			INSERT INTO ent_users (id, display_name, directory_status, employment, platform_role)
			VALUES ($1,$2,'pending_bind','inactive','')`, u.ID, u.Name); err != nil {
			return User{}, err
		}
	} else if err != nil {
		return User{}, err
	}
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO ent_identities (id, user_id, issuer, subject)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (issuer, subject) DO NOTHING`, uuid.NewString(), u.ID, issuer, subject); err != nil {
		return User{}, err
	}
	return s.userByID(ctx, u.ID)
}

func (s *Service) StartSession(ctx context.Context, userID string) (string, time.Time, error) {
	id := uuid.NewString()
	now := s.now()
	abs := now.Add(AbsoluteTimeout)
	idle := now.Add(IdleTimeout)
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO ent_sessions (id, user_id, created_at, last_seen, absolute_expires, idle_expires)
		VALUES ($1,$2,$3,$3,$4,$5)`, id, userID, now, abs, idle)
	return id, abs, err
}

// TouchSession 校验空闲超时、绝对超时、撤销和停用。停用后不能再取项目数据。
func (s *Service) TouchSession(ctx context.Context, sessionID string) (Actor, error) {
	now := s.now()
	var userID string
	var project sql.NullString
	var revoked sql.NullTime
	var abs, idle, lastSeen time.Time
	err := s.DB.QueryRowContext(ctx, `
		SELECT user_id, revoked_at, absolute_expires, idle_expires, project_id, last_seen
		FROM ent_sessions WHERE id=$1`, sessionID).Scan(&userID, &revoked, &abs, &idle, &project, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) || revoked.Valid {
		return Actor{}, fail(401, "session_ended", "登录已失效，请重新使用企业账号登录", nil)
	}
	if err != nil {
		return Actor{}, err
	}
	if now.After(abs) {
		return Actor{}, fail(401, "session_absolute", "登录已超过 8 小时，请重新使用企业账号登录", nil)
	}
	if now.After(idle) {
		return Actor{}, fail(401, "session_idle", "登录已空闲超过 30 分钟，请重新使用企业账号登录", nil)
	}
	u, err := s.userByID(ctx, userID)
	if err != nil {
		return Actor{}, err
	}
	if now.Sub(lastSeen) >= sessionTouchInterval {
		if _, err := s.DB.ExecContext(ctx, `
			UPDATE ent_sessions SET last_seen=$2, idle_expires=$3
			WHERE id=$1 AND revoked_at IS NULL AND last_seen <= $4`,
			sessionID, now, now.Add(IdleTimeout), now.Add(-sessionTouchInterval)); err != nil {
			return Actor{}, err
		}
	}
	return Actor{User: u, SessionID: sessionID, SessionProject: project.String}, nil
}

func (s *Service) Logout(ctx context.Context, sessionID string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE ent_sessions SET revoked_at=$2 WHERE id=$1`, sessionID, s.now())
	return err
}

type ProjectBrief struct {
	ID     string   `json:"id"`
	Code   string   `json:"code"`
	Name   string   `json:"name"`
	Role   string   `json:"role"`
	Post   string   `json:"post"`
	Duties []string `json:"duties"`
}

type Receipt struct {
	ID               string `json:"id"`
	ReceiptNo        string `json:"receipt_no"`
	ProjectCode      string `json:"project_code"`
	Status           string `json:"status"`
	StatusLabel      string `json:"status_label"`
	Purpose          string `json:"purpose,omitempty"`
	CredentialNotice string `json:"credential_notice,omitempty"`
	ProjectID        string `json:"project_id,omitempty"`
	ProjectName      string `json:"project_name,omitempty"`
	Reused           bool   `json:"reused"`
	StructuredReason string `json:"structured_reason,omitempty"`
}

type Me struct {
	Gate        string         `json:"gate"`
	Name        string         `json:"name"`
	UserID      string         `json:"user_id"`
	Platform    bool           `json:"platform"`
	Projects    []ProjectBrief `json:"projects"`
	Receipts    []Receipt      `json:"receipts"`
	InitGrant   string         `json:"init_grant,omitempty"`
	GrantReady  bool           `json:"grant_ready"`
	ContactName string         `json:"contact_name"`
}

func (s *Service) Me(ctx context.Context, a Actor) (Me, error) {
	me := Me{
		Gate: a.Gate(), Name: a.Name, UserID: a.ID, Platform: a.Platform(),
		Projects: []ProjectBrief{}, Receipts: []Receipt{}, ContactName: "林衡",
	}
	if a.Platform() {
		var note string
		var status string
		err := s.DB.QueryRowContext(ctx, `SELECT status, note FROM ent_init_grants WHERE id='init-demo'`).Scan(&status, &note)
		if err == nil && status == "approved" {
			me.GrantReady = true
			me.InitGrant = note
		}
	}
	if a.Gate() != "ready" {
		return me, nil
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT p.id, p.code, p.name, m.role, m.post
		FROM ent_members m JOIN ent_projects p ON p.id=m.project_id
		WHERE m.user_id=$1 AND p.status='active'
		ORDER BY p.code`, a.ID)
	if err != nil {
		return Me{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var p ProjectBrief
		var stored string
		if err := rows.Scan(&p.ID, &p.Code, &p.Name, &p.Role, &stored); err != nil {
			return Me{}, err
		}
		p.Duties, err = s.duties(ctx, p.ID, a.ID)
		if err != nil {
			return Me{}, err
		}
		p.Post = effectivePost(p.Role, p.Duties, stored)
		me.Projects = append(me.Projects, p)
	}
	recs, err := s.ownReceipts(ctx, a.ID)
	if err != nil {
		return Me{}, err
	}
	me.Receipts = recs
	return me, rows.Err()
}

func (s *Service) duties(ctx context.Context, projectID, userID string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT duty FROM ent_duties WHERE project_id=$1 AND user_id=$2 ORDER BY duty`, projectID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if out == nil {
		out = []string{}
	}
	return out, rows.Err()
}

func (s *Service) ownReceipts(ctx context.Context, userID string) ([]Receipt, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT r.id, r.receipt_no, r.project_code, r.status, r.purpose_display, r.credential_removed,
		       COALESCE(r.project_id,''), COALESCE(p.name,''), r.structured_reason
		FROM ent_join_requests r
		LEFT JOIN ent_projects p ON p.id=r.project_id AND r.status='approved'
		WHERE r.applicant_id=$1
		ORDER BY r.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Receipt
	for rows.Next() {
		var rc Receipt
		var removed bool
		if err := rows.Scan(&rc.ID, &rc.ReceiptNo, &rc.ProjectCode, &rc.Status, &rc.Purpose, &removed, &rc.ProjectID, &rc.ProjectName, &rc.StructuredReason); err != nil {
			return nil, err
		}
		rc.StatusLabel = joinStatusLabel(rc.Status)
		if removed {
			rc.CredentialNotice = CredentialNotice
		}
		if rc.Status != "approved" {
			rc.ProjectID = ""
			rc.ProjectName = ""
		}
		out = append(out, rc)
	}
	if out == nil {
		out = []Receipt{}
	}
	return out, rows.Err()
}

func joinStatusLabel(status string) string {
	switch status {
	case "pending":
		return "待处理"
	case "approved":
		return "已批准"
	case "rejected":
		return "已拒绝"
	default:
		return status
	}
}

func normalizeCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

func (s *Service) audit(ctx context.Context, tx *sql.Tx, projectID, actor, action, structured, summary, subjectType, subjectID, contentHash string, versionNo *int) error {
	var proj any
	if projectID != "" {
		proj = projectID
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO ent_audit (id, project_id, actor_id, action, structured_reason, summary, subject_type, subject_id, version_no, content_hash)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		uuid.NewString(), proj, actor, action, structured, summary, subjectType, subjectID, versionNo, contentHash)
	return err
}

func (s *Service) openTodo(ctx context.Context, tx *sql.Tx, kind, projectID, assignee, subjectType, subjectID, preview string) error {
	var proj any
	if projectID != "" {
		proj = projectID
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO ent_todos (id, kind, project_id, assignee_id, subject_type, subject_id, preview, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'open')
		ON CONFLICT (kind, assignee_id, subject_id) WHERE status='open' DO NOTHING`,
		uuid.NewString(), kind, proj, assignee, subjectType, subjectID, preview)
	return err
}

func (s *Service) closeTodos(ctx context.Context, tx *sql.Tx, subjectID string, kinds ...string) error {
	for _, kind := range kinds {
		if _, err := tx.ExecContext(ctx, `UPDATE ent_todos SET status='closed' WHERE subject_id=$1 AND kind=$2 AND status='open'`, subjectID, kind); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) nameOf(ctx context.Context, tx *sql.Tx, id string) string {
	var name string
	q := `SELECT display_name FROM ent_users WHERE id=$1`
	var err error
	if tx != nil {
		err = tx.QueryRowContext(ctx, q, id).Scan(&name)
	} else {
		err = s.DB.QueryRowContext(ctx, q, id).Scan(&name)
	}
	if err != nil || name == "" {
		return "另一位操作者"
	}
	return name
}

func (s *Service) Runtime(ctx context.Context) (running, queued int, err error) {
	err = s.DB.QueryRowContext(ctx, `
		SELECT
			count(*) FILTER (WHERE status='running' AND NOT paused AND NOT queued),
			count(*) FILTER (WHERE queued)
		FROM tasks`).Scan(&running, &queued)
	return running, queued, err
}

func idsFromJSON(raw []byte) []string {
	if len(raw) == 0 {
		return []string{}
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return []string{}
	}
	if ids == nil {
		return []string{}
	}
	return ids
}

func idsToJSON(ids []string) string {
	if ids == nil {
		ids = []string{}
	}
	b, _ := json.Marshal(ids)
	return string(b)
}

func hasID(ids []string, id string) bool {
	for _, item := range ids {
		if item == id {
			return true
		}
	}
	return false
}

func unionIDs(parts ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, part := range parts {
		for _, id := range part {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func avoidance(editors, lineage []string) []string {
	return unionIDs(editors, lineage)
}

func newID() string { return uuid.NewString() }

func randomReceipt() string {
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("JR-%X", buf)
}
