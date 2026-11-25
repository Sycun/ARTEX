package traffic

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// SQLiteStore is the IndexStore over the sidecar schema Traffic already uses.
type SQLiteStore struct {
	db  *sql.DB
	dir string
}

func NewSQLiteStore(db *sql.DB, dir string) *SQLiteStore {
	return &SQLiteStore{db: db, dir: dir}
}

func (s *SQLiteStore) InsertExchange(ctx context.Context, ex ExchangeWrite) error {
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `DELETE FROM blob_refs WHERE exchange_id=?`, ex.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT OR REPLACE INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,?,?,?,?,?,?,?,'')`,
		ex.ID, ex.TS.Unix(), ex.Host, ex.Method, ex.URLTemplate, ex.URL, ex.Status, ex.ContentType, ex.ReqLen, ex.RespLen); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT OR REPLACE INTO exchange_bodies(id,req_head,req_body,req_blob,resp_head,resp_body,resp_blob)
VALUES(?,?,?,NULL,?,?,NULL)`,
		ex.ID, ex.ReqHead, ex.ReqBody, ex.RespHead, ex.RespBody); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) ListExchanges(ctx context.Context, q ListQuery) ([]ExchangeMeta, int, error) {
	q.Page, q.Size = normalizePage(q.Page, q.Size)
	a := &sqlArgs{}
	where, err := whereSQL(q, a, sqliteExchangeCols())
	if err != nil {
		return nil, 0, err
	}
	from := ` FROM exchanges e LEFT JOIN exchange_bodies b ON b.id=e.id`
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+from+where, a.vals...).Scan(&total); err != nil {
		return nil, 0, err
	}
	c := sqliteExchangeCols()
	query := `SELECT e.id,e.ts,e.host,e.method,COALESCE(e.url_template,''),e.url,e.status,COALESCE(e.content_type,''),e.resp_len,COALESCE(e.path,'')` +
		from + where + ` ORDER BY ` + orderSQL(q.Sort, q.Order, c) + ` LIMIT ? OFFSET ?`
	args := append(append([]any{}, a.vals...), q.Size, q.Page*q.Size)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []ExchangeMeta
	for rows.Next() {
		var m ExchangeMeta
		if err := rows.Scan(&m.ID, &m.TS, &m.Host, &m.Method, &m.URLTemplate, &m.URL, &m.Status, &m.ContentType, &m.RespLen, &m.Path); err != nil {
			return nil, 0, err
		}
		out = append(out, m)
	}
	return out, total, rows.Err()
}

func (s *SQLiteStore) GetExchange(ctx context.Context, id string) (ExchangeRead, error) {
	var ex ExchangeRead
	var req, resp []byte
	err := s.db.QueryRowContext(ctx, `
SELECT e.id,e.ts,e.host,e.method,COALESCE(e.url_template,''),e.url,e.status,COALESCE(e.content_type,''),
       e.resp_len,COALESCE(e.path,''),e.req_len,COALESCE(b.req_head,''),COALESCE(b.resp_head,'')
FROM exchanges e JOIN exchange_bodies b ON b.id=e.id WHERE e.id=?`, id).
		Scan(&ex.ID, &ex.TS, &ex.Host, &ex.Method, &ex.URLTemplate, &ex.URL, &ex.Status, &ex.ContentType,
			&ex.RespLen, &ex.Path, &ex.ReqLen, &ex.ReqHead, &ex.RespHead)
	if err != nil {
		return ExchangeRead{}, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT req_body, resp_body FROM exchange_bodies WHERE id=?`, id).Scan(&req, &resp)
	if err != nil {
		return ExchangeRead{}, err
	}
	ex.ReqBody = req
	ex.RespBody = resp
	return ex, nil
}

func (s *SQLiteStore) HostStats(ctx context.Context, scope []string) ([]HostCount, error) {
	a := &sqlArgs{}
	where, err := whereSQL(ListQuery{Hosts: scope, RespMin: -1, RespMax: -1}, a, sqliteExchangeCols())
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.host, COUNT(*), MAX(e.ts) FROM exchanges e`+where+` GROUP BY e.host ORDER BY MAX(e.ts) DESC`, a.vals...)
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

func (s *SQLiteStore) TemplateStats(ctx context.Context, host string) ([]TemplateStat, error) {
	q := `SELECT host, COALESCE(url_template,''), COUNT(*) FROM exchanges`
	var args []any
	if host != "" {
		q += ` WHERE host=?`
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

func (s *SQLiteStore) DeleteHost(ctx context.Context, host string) (int64, error) {
	return s.deleteWhere(ctx, `host LIKE ?`, "%"+host+"%")
}

func (s *SQLiteStore) DeleteHosts(ctx context.Context, hosts []string) (int64, error) {
	if len(hosts) == 0 {
		return 0, nil
	}
	a := &sqlArgs{}
	ors := make([]string, len(hosts))
	for i, h := range hosts {
		ors[i] = "host = " + a.ph(h)
	}
	return s.deleteWhere(ctx, strings.Join(ors, " OR "), a.vals...)
}

func (s *SQLiteStore) DeleteExchange(ctx context.Context, id string) error {
	_, err := s.deleteWhere(ctx, `id=?`, id)
	return err
}

func (s *SQLiteStore) DeleteAll(ctx context.Context) (int64, error) {
	return s.deleteWhere(ctx, `1=1`)
}

func (s *SQLiteStore) BlobRefCount(ctx context.Context, blobKey string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blob_refs WHERE hash=?`, blobKey).Scan(&n)
	return n, err
}

func (s *SQLiteStore) deleteWhere(ctx context.Context, where string, args ...any) (int64, error) {
	fts, err := s.hasFTS(ctx)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	hashes, err := sqliteHashes(ctx, tx, where, args)
	if err != nil {
		return 0, err
	}
	if fts {
		if _, err := tx.ExecContext(ctx, `DELETE FROM ex_fts WHERE rowid IN (SELECT rowid FROM exchanges WHERE `+where+`)`, args...); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM exchange_bodies WHERE id IN (SELECT id FROM exchanges WHERE `+where+`)`, args...); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM blob_refs WHERE exchange_id IN (SELECT id FROM exchanges WHERE `+where+`)`, args...); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM exchanges WHERE `+where, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if err := s.removeUnreferenced(ctx, hashes); err != nil {
		return n, err
	}
	return n, nil
}

func sqliteHashes(ctx context.Context, tx *sql.Tx, where string, args []any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT COALESCE(req_blob,''), COALESCE(resp_blob,'') FROM exchange_bodies
WHERE id IN (SELECT id FROM exchanges WHERE `+where+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var req, resp string
		if err := rows.Scan(&req, &resp); err != nil {
			return nil, err
		}
		if req != "" {
			out = append(out, req)
		}
		if resp != "" {
			out = append(out, resp)
		}
	}
	return out, rows.Err()
}

func (s *SQLiteStore) removeUnreferenced(ctx context.Context, hashes []string) error {
	seen := map[string]struct{}{}
	for _, hash := range hashes {
		if _, ok := seen[hash]; ok {
			continue
		}
		seen[hash] = struct{}{}
		n, err := s.BlobRefCount(ctx, hash)
		if err != nil {
			return err
		}
		if n > 0 || s.dir == "" || len(hash) < 4 {
			continue
		}
		for _, p := range []string{
			filepath.Join(s.dir, "_blobs", "sha256", hash[:2], hash+".bin"),
			filepath.Join(s.dir, "_blobs", "sha256", hash[:2], hash[2:4], hash+".bin"),
		} {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func (s *SQLiteStore) hasFTS(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='ex_fts'`).Scan(&n)
	return n > 0, err
}
