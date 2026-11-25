package traffic

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io/fs"

	"github.com/Autumn-27/artex/storage"
)

// PGStore stores exchange metadata in PostgreSQL and deduplicated bodies in a BlobStore.
type PGStore struct {
	db    *sql.DB
	blobs storage.BlobStore
}

func NewPGStore(db *sql.DB, blobs storage.BlobStore) (*PGStore, error) {
	if db == nil || blobs == nil {
		return nil, errors.New("traffic pg store requires db and blob store")
	}
	return &PGStore{db: db, blobs: blobs}, nil
}

func (s *PGStore) InsertExchange(ctx context.Context, ex ExchangeWrite) error {
	if ex.ID == "" {
		return errors.New("exchange id is empty")
	}
	if ex.TS.IsZero() {
		return errors.New("exchange ts is empty")
	}
	if ex.ReqLen == 0 {
		ex.ReqLen = len(ex.ReqBody)
	}
	if ex.RespLen == 0 {
		ex.RespLen = len(ex.RespBody)
	}
	reqKey, err := s.putBody(ctx, ex.ReqBody)
	if err != nil {
		return err
	}
	respKey, err := s.putBody(ctx, ex.RespBody)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	var oldReq, oldResp string
	err = tx.QueryRowContext(ctx, `SELECT req_blob_key, resp_blob_key FROM traffic_exchanges WHERE id=$1`, ex.ID).Scan(&oldReq, &oldResp)
	missing := errors.Is(err, sql.ErrNoRows)
	if err != nil && !missing {
		return err
	}
	if missing {
		_, err = tx.ExecContext(ctx, `
INSERT INTO traffic_exchanges(
    id, task_id, project_id, ts, host, method, url_template, url, status, content_type,
    req_len, resp_len, req_head, resp_head, req_blob_key, resp_blob_key)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
			ex.ID, ex.TaskID, ex.ProjectID, ex.TS.UTC(), ex.Host, ex.Method, ex.URLTemplate, ex.URL, ex.Status, ex.ContentType,
			ex.ReqLen, ex.RespLen, ex.ReqHead, ex.RespHead, reqKey, respKey)
	} else {
		_, err = tx.ExecContext(ctx, `
UPDATE traffic_exchanges SET
    task_id=$2, project_id=$3, ts=$4, host=$5, method=$6, url_template=$7, url=$8, status=$9,
    content_type=$10, req_len=$11, resp_len=$12, req_head=$13, resp_head=$14, req_blob_key=$15, resp_blob_key=$16
WHERE id=$1`,
			ex.ID, ex.TaskID, ex.ProjectID, ex.TS.UTC(), ex.Host, ex.Method, ex.URLTemplate, ex.URL, ex.Status, ex.ContentType,
			ex.ReqLen, ex.RespLen, ex.ReqHead, ex.RespHead, reqKey, respKey)
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.releaseBlobs(ctx, []string{oldReq, oldResp})
}

func (s *PGStore) putBody(ctx context.Context, body []byte) (string, error) {
	if len(body) == 0 {
		return "", nil
	}
	sum := sha256.Sum256(body)
	key := "traffic/blobs/" + hex.EncodeToString(sum[:])
	if _, err := s.blobs.Stat(ctx, key); err == nil {
		return key, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := s.blobs.Put(ctx, key, body); err != nil {
		return "", err
	}
	return key, nil
}

func (s *PGStore) ListExchanges(ctx context.Context, q ListQuery) ([]ExchangeMeta, int, error) {
	q.Page, q.Size = normalizePage(q.Page, q.Size)
	a := &sqlArgs{pg: true}
	where, err := whereSQL(q, a, pgExchangeCols())
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM traffic_exchanges`+where, a.vals...).Scan(&total); err != nil {
		return nil, 0, err
	}
	c := pgExchangeCols()
	limit := a.ph(q.Size)
	offset := a.ph(q.Page * q.Size)
	query := `SELECT id, (extract(epoch FROM ts))::bigint, host, method, url_template, url, status, content_type, resp_len
FROM traffic_exchanges` + where + ` ORDER BY ` + orderSQL(q.Sort, q.Order, c) + ` LIMIT ` + limit + ` OFFSET ` + offset
	rows, err := s.db.QueryContext(ctx, query, a.vals...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []ExchangeMeta
	for rows.Next() {
		var m ExchangeMeta
		if err := rows.Scan(&m.ID, &m.TS, &m.Host, &m.Method, &m.URLTemplate, &m.URL, &m.Status, &m.ContentType, &m.RespLen); err != nil {
			return nil, 0, err
		}
		out = append(out, m)
	}
	return out, total, rows.Err()
}

func (s *PGStore) GetExchange(ctx context.Context, id string) (ExchangeRead, error) {
	var ex ExchangeRead
	err := s.db.QueryRowContext(ctx, `
SELECT id, (extract(epoch FROM ts))::bigint, host, method, url_template, url, status, content_type, resp_len,
       task_id, project_id, req_len, req_head, resp_head, req_blob_key, resp_blob_key
FROM traffic_exchanges WHERE id=$1`, id).
		Scan(&ex.ID, &ex.TS, &ex.Host, &ex.Method, &ex.URLTemplate, &ex.URL, &ex.Status, &ex.ContentType, &ex.RespLen,
			&ex.TaskID, &ex.ProjectID, &ex.ReqLen, &ex.ReqHead, &ex.RespHead, &ex.ReqBlobKey, &ex.RespBlobKey)
	if err != nil {
		return ExchangeRead{}, err
	}
	ex.ReqBody, err = s.readKey(ctx, ex.ReqBlobKey)
	if err != nil {
		return ExchangeRead{}, err
	}
	ex.RespBody, err = s.readKey(ctx, ex.RespBlobKey)
	if err != nil {
		return ExchangeRead{}, err
	}
	return ex, nil
}

func (s *PGStore) readKey(ctx context.Context, key string) ([]byte, error) {
	if key == "" {
		return nil, nil
	}
	return s.blobs.Get(ctx, key)
}

func (s *PGStore) HostStats(ctx context.Context, scope []string) ([]HostCount, error) {
	a := &sqlArgs{pg: true}
	where, err := whereSQL(ListQuery{Hosts: scope, RespMin: -1, RespMax: -1}, a, pgExchangeCols())
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT host, COUNT(*), (extract(epoch FROM MAX(ts)))::bigint FROM traffic_exchanges`+where+` GROUP BY host ORDER BY MAX(ts) DESC`, a.vals...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostCount
	for rows.Next() {
		var h HostCount
		var last int64
		if err := rows.Scan(&h.Host, &h.Count, &last); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *PGStore) TemplateStats(ctx context.Context, host string) ([]TemplateStat, error) {
	q := `SELECT host, url_template, COUNT(*) FROM traffic_exchanges`
	var args []any
	if host != "" {
		q += ` WHERE host=$1`
		args = append(args, host)
	}
	q += ` GROUP BY host, url_template ORDER BY COUNT(*) DESC, url_template`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TemplateStat
	for rows.Next() {
		var st TemplateStat
		if err := rows.Scan(&st.Host, &st.URLTemplate, &st.Count); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *PGStore) DeleteHost(ctx context.Context, host string) (int64, error) {
	return s.deleteWhere(ctx, `host ILIKE $1`, "%"+host+"%")
}

func (s *PGStore) DeleteHosts(ctx context.Context, hosts []string) (int64, error) {
	if len(hosts) == 0 {
		return 0, nil
	}
	a := &sqlArgs{pg: true}
	ors := make([]string, len(hosts))
	for i, h := range hosts {
		ors[i] = "host = " + a.ph(h)
	}
	return s.deleteWhere(ctx, joinOr(ors), a.vals...)
}

func (s *PGStore) DeleteExchange(ctx context.Context, id string) error {
	_, err := s.deleteWhere(ctx, `id=$1`, id)
	return err
}

func (s *PGStore) DeleteAll(ctx context.Context) (int64, error) {
	return s.deleteWhere(ctx, `true`)
}

func (s *PGStore) BlobRefCount(ctx context.Context, blobKey string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
SELECT (SELECT COUNT(*) FROM traffic_exchanges WHERE req_blob_key=$1)
     + (SELECT COUNT(*) FROM traffic_exchanges WHERE resp_blob_key=$1)`, blobKey).Scan(&n)
	return n, err
}

func (s *PGStore) deleteWhere(ctx context.Context, where string, args ...any) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	rows, err := tx.QueryContext(ctx, `DELETE FROM traffic_exchanges WHERE `+where+` RETURNING req_blob_key, resp_blob_key`, args...)
	if err != nil {
		return 0, err
	}
	var keys []string
	var n int64
	for rows.Next() {
		var req, resp string
		if err := rows.Scan(&req, &resp); err != nil {
			rows.Close()
			return 0, err
		}
		n++
		keys = append(keys, req, resp)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if err := s.releaseBlobs(ctx, keys); err != nil {
		return n, err
	}
	return n, nil
}

func (s *PGStore) releaseBlobs(ctx context.Context, keys []string) error {
	seen := map[string]struct{}{}
	for _, key := range keys {
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		n, err := s.BlobRefCount(ctx, key)
		if err != nil {
			return err
		}
		if n == 0 {
			if err := s.blobs.Delete(ctx, key); err != nil {
				return err
			}
		}
	}
	return nil
}

var (
	_ IndexStore = (*SQLiteStore)(nil)
	_ IndexStore = (*PGStore)(nil)
)

func joinOr(parts []string) string {
	out := parts[0]
	for _, p := range parts[1:] {
		out += " OR " + p
	}
	return out
}
