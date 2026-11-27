package traffic

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	mproxy "github.com/lqqyt2423/go-mitmproxy/proxy"

	"github.com/Autumn-27/artex/traffic/rewrite"
)

// requestView is the request an exchange records: what the client sent, which
// is not the flow's request once a rule rewrote it.
type requestView struct {
	method  string
	url     *url.URL
	proto   string
	header  http.Header
	rawHost string
	body    []byte
	// unavailable marks a rewritten request whose original was not kept.
	unavailable bool
}

// responseView is the response an exchange records: what the target answered.
type responseView struct {
	status int
	header http.Header
	body   []byte
}

func flowRequestView(r *mproxy.Request) requestView {
	if r == nil {
		return requestView{}
	}
	v := requestView{method: r.Method, url: r.URL, proto: r.Proto, header: r.Header, body: r.Body}
	if raw := r.Raw(); raw != nil {
		v.rawHost = raw.Host
	}
	return v
}

func flowResponseView(r *mproxy.Response) responseView {
	if r == nil {
		return responseView{header: http.Header{}}
	}
	return responseView{status: r.StatusCode, header: r.Header, body: r.Body}
}

const unavailableMarker = "[original request unavailable]\n"

// recordViews writes one exchange and its rewrite trace. When a PostgreSQL
// index is installed, that store is authoritative. The sidecar SQLite path
// queues full-text indexing instead of doing it on the request path.
func (t *Traffic) recordViews(rq requestView, rs responseView, lines []rewrite.TraceLine) {
	if t == nil {
		return
	}
	if t.index != nil {
		t.recordViewsIndex(rq, rs, lines)
		return
	}
	if job := t.writeViews(rq, rs, lines); job != nil && t.ftsq != nil {
		t.ftsq.add(*job)
	}
}

func (t *Traffic) recordViewsIndex(rq requestView, rs responseView, lines []rewrite.TraceLine) {
	var host, tmpl, rawURL, uri string
	if rq.url != nil {
		host = rq.url.Hostname()
		tmpl = db.TemplatePath(rq.url.EscapedPath())
		rawURL = rq.url.String()
		uri = rq.url.RequestURI()
	}
	now := time.Now()
	ct := ""
	if rs.header != nil {
		ct = rs.header.Get("Content-Type")
	}
	rewritten, _ := appliedRules(lines)
	ex := ExchangeWrite{
		ID: t.newExchangeID(now), TS: now, Host: host, Method: rq.method,
		URLTemplate: tmpl, URL: rawURL, Status: rs.status, ContentType: ct,
		ReqLen: len(rq.body), RespLen: len(rs.body),
		ReqHead: requestHead(rq, uri), RespHead: fmt.Sprintf("HTTP %d\n%s", rs.status, headerLines(rs.header)),
		ReqBody: append([]byte(nil), rq.body...), RespBody: append([]byte(nil), rs.body...),
		Rewritten: rewritten, Rewrites: traceWrites(lines),
	}
	if err := t.index.InsertExchange(context.Background(), ex); err != nil {
		log.Printf("[traffic] 记录 %s 失败: %v", rawURL, err)
	}
}

func (t *Traffic) writeViews(rq requestView, rs responseView, lines []rewrite.TraceLine) *ftsJob {
	reqIdx, reqDecoded := indexBody(rq.header, rq.body)
	respIdx, respDecoded := indexBody(rs.header, rs.body)
	t.wmu.Lock()
	defer t.wmu.Unlock()
	var host, tmpl, rawURL, uri string
	if rq.url != nil {
		host = rq.url.Hostname()
		tmpl = db.TemplatePath(rq.url.EscapedPath())
		rawURL = rq.url.String()
		uri = rq.url.RequestURI()
	}
	now := time.Now()
	id := t.newExchangeID(now)
	ct := ""
	if rs.header != nil {
		ct = rs.header.Get("Content-Type")
	}
	reqCT := ""
	if rq.header != nil {
		reqCT = rq.header.Get("Content-Type")
	}
	reqHead := requestHead(rq, uri)
	respHead := fmt.Sprintf("HTTP %d\n%s", rs.status, headerLines(rs.header))
	reqB := t.spill(rq.body, reqCT)
	respB := t.spill(rs.body, ct)
	if reqDecoded {
		reqB.index = reqIdx
	}
	if respDecoded {
		respB.index = respIdx
	}
	trace := t.spillTrace(lines)
	rewritten, ruleIDs := appliedRules(lines)
	fail := func(step string, err error) {
		if rewritten {
			slog.Error("[traffic] failed to record rewritten exchange", "host", host, "step", step, "rules", ruleIDs)
			return
		}
		log.Printf("[traffic] 记录 %s 失败（%s）：%v", rawURL, step, err)
	}
	tx, err := t.db.Begin()
	if err != nil {
		fail("开启事务", err)
		return nil
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec(`DELETE FROM exchange_rewrites WHERE exchange_id=?`, id); err != nil {
		fail("清理改写记录", err)
		return nil
	}
	res, err := tx.Exec(`INSERT OR REPLACE INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,?,?,?,?,?,?,?,'')`,
		id, now.Unix(), host, rq.method, tmpl, rawURL, rs.status, ct, len(rq.body), len(rs.body))
	if err != nil {
		fail("写索引", err)
		return nil
	}
	rowid, err := res.LastInsertId()
	if err != nil {
		fail("取 rowid", err)
		return nil
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO exchange_bodies(id,req_head,req_body,req_blob,resp_head,resp_body,resp_blob)
VALUES(?,?,?,?,?,?,?)`,
		id, reqHead, reqB.inline, nullIfEmpty(reqB.hash), respHead, respB.inline, nullIfEmpty(respB.hash)); err != nil {
		fail("写正文", err)
		return nil
	}
	for _, h := range []string{reqB.hash, respB.hash} {
		if h == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO blob_refs(hash,exchange_id) VALUES(?,?)`, h, id); err != nil {
			fail("登记 blob 引用", err)
			return nil
		}
	}
	if err := writeTrace(tx, id, lines, trace); err != nil {
		fail("写改写记录", err)
		return nil
	}
	var job *ftsJob
	if t.fts {
		idx := strings.Join([]string{rawURL, reqHead, reqB.index, respHead, respB.index}, "\n")
		if t.ftsq != nil {
			if _, err := tx.Exec(`INSERT OR REPLACE INTO ex_fts_pending(rowid,id,content) VALUES(?,?,?)`, rowid, id, idx); err != nil {
				fail("排队全文索引", err)
				return nil
			}
			job = &ftsJob{rowid: rowid, id: id}
		} else if _, err := tx.Exec(`INSERT INTO ex_fts(rowid,content) VALUES(?,?)`, rowid, idx); err != nil {
			fail("写全文索引", err)
			return nil
		}
	}
	if err := tx.Commit(); err != nil {
		fail("提交", err)
		return nil
	}
	return job
}

func appliedRules(lines []rewrite.TraceLine) (bool, []int64) {
	rewritten := false
	var ids []int64
	for _, l := range lines {
		if l.Outcome != rewrite.OutcomeApplied {
			continue
		}
		rewritten = true
		if l.RuleID != 0 && !slices.Contains(ids, l.RuleID) {
			ids = append(ids, l.RuleID)
		}
	}
	return rewritten, ids
}

func traceWrites(lines []rewrite.TraceLine) []RewriteTraceWrite {
	if len(lines) == 0 {
		return nil
	}
	out := make([]RewriteTraceWrite, len(lines))
	for i, l := range lines {
		kind := l.SourceKind
		if kind == "" {
			kind = rewrite.SourceKindRule
		}
		out[i] = RewriteTraceWrite{
			Seq: i, Phase: string(l.Phase), SourceKind: kind,
			RuleID: l.RuleID, HookID: l.HookID, HookVersion: l.HookVersion,
			RuleSnapshot: l.RuleSnapshot, Action: l.Action, Field: l.Field,
			Outcome: l.Outcome, Reason: l.Reason,
			BeforeHash: l.BeforeHash, AfterHash: l.AfterHash, AfterInline: append([]byte(nil), l.After...),
		}
	}
	return out
}

func (t *Traffic) newExchangeID(now time.Time) string {
	n := t.seq.Add(1)
	return fmt.Sprintf("%d-%04d", now.Unix(), n%10000)
}

func requestHead(rq requestView, target string) string {
	urlHost := ""
	if rq.url != nil {
		urlHost = rq.url.Host
	}
	if rq.unavailable {
		return fmt.Sprintf("%s %s %s\n%s%s", rq.method, target, rq.proto, requestHeaderLinesOf(nil, urlHost, rq.rawHost), unavailableMarker)
	}
	return fmt.Sprintf("%s %s %s\n%s", rq.method, target, rq.proto, requestHeaderLinesOf(rq.header, urlHost, rq.rawHost))
}

func requestHeaderLinesOf(header http.Header, urlHost, rawHost string) string {
	headers := header.Clone()
	if headers == nil {
		headers = http.Header{}
	}
	headers.Del("Host")
	if headers.Get("User-Agent") == "" {
		headers.Del("User-Agent")
	}
	host := urlHost
	if strings.TrimSpace(rawHost) != "" {
		host = rawHost
	}
	var b strings.Builder
	if host = strings.TrimSpace(host); host != "" {
		b.WriteString("Host: ")
		b.WriteString(host)
		b.WriteByte('\n')
	}
	b.WriteString(headerLines(headers))
	return b.String()
}

// storeDir reports whether name is one of the traffic store's own directories.
func storeDir(name string) bool {
	switch name {
	case "_index", "_blobs", "_ca", "_delete_staging":
		return true
	default:
		return false
	}
}

// hostDir is the legacy on-disk tree for one recorded host. Reserved store
// directories and path escapes are refused; callers still delete the index rows.
func hostDir(root, host string) (string, bool) {
	name := sanitize(host)
	if name == "." || name == ".." || storeDir(name) {
		return "", false
	}
	dir := filepath.Join(root, name)
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return dir, true
}

func clampBlobRange(offset, length, size int64) (off, n int64, empty bool) {
	if offset < 0 {
		offset = 0
	}
	if size <= 0 || offset >= size {
		return offset, 0, true
	}
	remain := size - offset
	if length <= 0 || length > remain {
		length = remain
	}
	return offset, length, false
}

// ResponseContaining reports the newest capture in [fromTS,toTS] whose response
// side contains one of lines. Request-side matches are ignored.
func (t *Traffic) ResponseContaining(lines []string, fromTS, toTS int64) (id string, ok bool, err error) {
	if t == nil || len(lines) == 0 || t.index != nil {
		return "", false, nil
	}
	for _, line := range lines {
		cond, arg, hasFTS := t.ftsFilter(line)
		if !hasFTS {
			return "", false, nil
		}
		rows, qerr := t.db.Query(`SELECT id FROM exchanges
WHERE ts>=? AND ts<=? AND `+cond+`
ORDER BY ts DESC LIMIT 8`, fromTS, toTS, arg)
		if qerr != nil {
			return "", false, qerr
		}
		ids := make([]string, 0, 8)
		for rows.Next() {
			var cand string
			if serr := rows.Scan(&cand); serr != nil {
				rows.Close()
				return "", false, serr
			}
			ids = append(ids, cand)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return "", false, err
		}
		for _, cand := range ids {
			_, resp, gerr := t.Get(cand)
			if gerr != nil {
				continue
			}
			if strings.Contains(resp, line) || t.decodedResponseContains(cand, line) {
				return cand, true, nil
			}
		}
	}
	return "", false, nil
}

func (t *Traffic) decodedResponseContains(id, line string) bool {
	_, _, respHead, respBody, err := t.loadFullExchange(id, "")
	if err != nil {
		return false
	}
	_, _, headers := parseStoredResponseHead(respHead)
	h := http.Header{}
	h.Set("Content-Encoding", findHeaderValue(headers, "Content-Encoding"))
	dec, ok := decodeBody(h, respBody, maxIndexBody)
	return ok && strings.Contains(string(dec), line)
}
