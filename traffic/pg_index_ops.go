package traffic

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/artex/traffic/rewrite"
)

func (t *Traffic) insertHARIndex(ex parsedHARExchange) (bool, error) {
	if _, err := t.index.GetExchange(context.Background(), ex.id); err == nil {
		return false, nil
	} else if err != nil && !isNotFound(err) {
		return false, err
	}
	ts := time.Unix(ex.ts, 0).UTC()
	err := t.index.InsertExchange(context.Background(), ExchangeWrite{
		ID: ex.id, TS: ts, Host: ex.host, Method: ex.method, URLTemplate: ex.urlTemplate, URL: ex.rawURL,
		Status: ex.status, ContentType: ex.respCT, ReqLen: len(ex.reqBody), RespLen: len(ex.respBody),
		ReqHead: ex.reqHead, RespHead: ex.respHead, ReqBody: ex.reqBody, RespBody: ex.respBody,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

func isNotFound(err error) bool {
	return err == sql.ErrNoRows || strings.Contains(strings.ToLower(err.Error()), "no rows")
}

func (t *Traffic) recordStreamStartIndex(rq requestView, rs responseView, kind string, lines []rewrite.TraceLine) (string, error) {
	now := time.Now()
	id := t.newExchangeID(now)
	method, display, tmpl, host, target := rq.method, "", "", "", ""
	if rq.url != nil {
		method, display, tmpl = streamDisplayURL(rq.method, rq.url, kind)
		host = rq.url.Hostname()
		target = rq.url.RequestURI()
		if strings.EqualFold(rq.method, "CONNECT") {
			target = rq.url.Host
		}
	}
	status, ct, respHead := 101, "websocket", "HTTP/1.1 101 Switching Protocols\n"
	if kind == "sse" {
		status = rs.status
		if rs.header != nil {
			ct = rs.header.Get("Content-Type")
		}
		respHead = fmt.Sprintf("HTTP %d\n%s", status, headerLines(rs.header))
	}
	rewritten, _ := appliedRules(lines)
	reqCT := ""
	if rq.header != nil {
		reqCT = rq.header.Get("Content-Type")
	}
	_ = reqCT
	err := t.index.InsertExchange(context.Background(), ExchangeWrite{
		ID: id, TS: now, Host: host, Method: method, URLTemplate: tmpl, URL: display,
		Status: status, ContentType: ct, ReqLen: len(rq.body), RespLen: 0,
		ReqHead: requestHead(rq, target), RespHead: respHead,
		ReqBody: append([]byte(nil), rq.body...), Rewritten: rewritten, Rewrites: traceWrites(lines),
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *PGStore) insertStreamMessage(exchangeID string, seq, ts int64, direction string, kind int, event, eventID string, retry, size int, payload []byte, blob string, truncated bool) error {
	_, err := s.db.ExecContext(context.Background(), `INSERT INTO traffic_stream_messages(
exchange_id, seq, ts, direction, kind, event, event_id, retry, size, payload, payload_blob, truncated)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		exchangeID, seq, ts, direction, kind, event, eventID, retry, size, payload, blob, truncated)
	return err
}

func (s *PGStore) finishStream(st *streamState) error {
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `UPDATE traffic_exchanges SET req_len=$2, resp_len=$3 WHERE id=$1`, st.id, st.reqBytes, st.respBytes); err != nil {
		return err
	}
	if st.dropped {
		meta := streamMetaPayload(st)
		_, err := s.db.ExecContext(ctx, `UPDATE traffic_stream_messages SET payload=$2, size=$3 WHERE exchange_id=$1 AND direction='meta'`, st.id, meta, len(meta))
		return err
	}
	return nil
}

func (s *PGStore) streamInfo(exchangeID string) (count int, kind string, err error) {
	var ct string
	err = s.db.QueryRowContext(context.Background(), `SELECT content_type FROM traffic_exchanges WHERE id=$1`, exchangeID).Scan(&ct)
	if err == sql.ErrNoRows {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", err
	}
	switch c := strings.ToLower(ct); {
	case c == "websocket":
		kind = "websocket"
	case strings.Contains(c, "text/event-stream"):
		kind = "sse"
	}
	err = s.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM traffic_stream_messages WHERE exchange_id=$1 AND direction <> 'meta'`, exchangeID).Scan(&count)
	return count, kind, err
}

func (s *PGStore) streamMessagePage(exchangeID, direction string, offset, limit int) (StreamMessagePage, error) {
	if _, _, err := streamDirectionFilter(direction); err != nil {
		return StreamMessagePage{}, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	args := []any{exchangeID}
	pgCond := ""
	switch direction {
	case "":
	case "c2s", "s2c":
		pgCond = " AND direction IN ($2, 'meta')"
		args = append(args, direction)
	}
	var page StreamMessagePage
	if err := s.db.QueryRowContext(context.Background(), `SELECT COUNT(*), COUNT(CASE WHEN direction <> 'meta' THEN 1 END)
FROM traffic_stream_messages WHERE exchange_id=$1`+pgCond, args...).Scan(&page.Total, &page.MessageTotal); err != nil {
		return StreamMessagePage{}, err
	}
	qargs := append(append([]any{}, args...), limit, offset)
	limitPh := fmt.Sprintf("$%d", len(args)+1)
	offsetPh := fmt.Sprintf("$%d", len(args)+2)
	rows, err := s.db.QueryContext(context.Background(), `SELECT seq,ts,direction,kind,event,event_id,retry,size,payload,payload_blob,truncated
FROM traffic_stream_messages WHERE exchange_id=$1`+pgCond+` ORDER BY seq LIMIT `+limitPh+` OFFSET `+offsetPh, qargs...)
	if err != nil {
		return StreamMessagePage{}, err
	}
	defer rows.Close()
	page.Items = []StreamMessage{}
	for rows.Next() {
		var m StreamMessage
		var payload []byte
		var blob sql.NullString
		var truncated bool
		if err := rows.Scan(&m.Seq, &m.TS, &m.Direction, &m.Kind, &m.Event, &m.EventID, &m.Retry, &m.Size, &payload, &blob, &truncated); err != nil {
			return StreamMessagePage{}, err
		}
		m.Truncated = truncated
		m.Blob = blob.String
		switch {
		case m.Direction == "meta":
			m.Type = "meta"
		case m.Kind == wsBinary:
			m.Type = "binary"
		case m.Kind == wsText:
			m.Type = "text"
		default:
			m.Type = "event"
		}
		if m.Type == "binary" {
			m.Payload = string(payload)
		} else {
			m.Payload = truncateUTF8(payload, streamPreview)
		}
		page.Items = append(page.Items, m)
	}
	return page, rows.Err()
}

func (s *PGStore) rewriteInfo(id string) (bool, []int64, error) {
	var rewritten bool
	if err := s.db.QueryRowContext(context.Background(), `SELECT rewritten FROM traffic_exchanges WHERE id=$1`, id).Scan(&rewritten); err != nil {
		if err == sql.ErrNoRows {
			return false, nil, nil
		}
		return false, nil, err
	}
	if !rewritten {
		return false, nil, nil
	}
	rows, err := s.db.QueryContext(context.Background(), `SELECT DISTINCT rule_id FROM exchange_rewrites WHERE exchange_id=$1 AND outcome='applied' AND rule_id IS NOT NULL ORDER BY rule_id`, id)
	if err != nil {
		return false, nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			return false, nil, err
		}
		ids = append(ids, rid)
	}
	return true, ids, rows.Err()
}

func (s *PGStore) recordRawTCP(t *Traffic, rec RawTCPCapture, previewBytes int) (string, error) {
	started := rec.StartedAt.UTC()
	if started.IsZero() {
		started = time.Now().UTC()
	}
	ended := rec.EndedAt.UTC()
	if ended.IsZero() || ended.Before(started) {
		ended = started
	}
	c2sPrev, c2sBlob := t.spillRawDir(rec.C2SData, previewBytes)
	s2cPrev, s2cBlob := t.spillRawDir(rec.S2CData, previewBytes)
	id := t.newRawTCPStreamID(started)
	_, err := s.db.ExecContext(context.Background(), `INSERT INTO raw_tcp_streams(
id, protocol, host, port, started_at, ended_at, duration_ms, c2s_bytes, s2c_bytes, truncated,
c2s_preview, c2s_blob, s2c_preview, s2c_blob)
VALUES ($1,'tcp',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		id, strings.ToLower(strings.TrimSpace(rec.Host)), rec.Port,
		started.UnixMilli(), ended.UnixMilli(), ended.Sub(started).Milliseconds(),
		rec.C2SBytes, rec.S2CBytes, rec.Truncated, c2sPrev, c2sBlob, s2cPrev, s2cBlob)
	if err != nil {
		return "", fmt.Errorf("insert raw_tcp_streams: %w", err)
	}
	return id, nil
}

func (s *PGStore) recordRawUDP(t *Traffic, rec RawUDPCapture, previewBytes int) (string, error) {
	dir := strings.ToLower(strings.TrimSpace(rec.Direction))
	if dir != "c2s" && dir != "s2c" {
		return "", fmt.Errorf("invalid raw udp direction %q", rec.Direction)
	}
	ts := rec.Timestamp.UTC()
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	preview, blob := t.spillRawDir(rec.Payload, previewBytes)
	id := t.newRawUDPDatagramID(ts)
	_, err := s.db.ExecContext(context.Background(), `INSERT INTO raw_udp_datagrams(
id, association_id, protocol, host, port, direction, ts, size, preview, blob)
VALUES ($1,$2,'udp',$3,$4,$5,$6,$7,$8,$9)`,
		id, strings.TrimSpace(rec.AssociationID), strings.ToLower(strings.TrimSpace(rec.Host)), rec.Port,
		dir, ts.UnixMilli(), int64(len(rec.Payload)), preview, blob)
	if err != nil {
		return "", fmt.Errorf("insert raw_udp_datagrams: %w", err)
	}
	return id, nil
}

func (t *Traffic) exportIndexedHAR(f PageQuery, opts HARExportOptions, creatorVer string) (HAR, HARExportMeta, error) {
	ctx := context.Background()
	q := ListQuery{
		Host: f.Host, HostLike: strings.TrimSpace(f.Host) != "", Hosts: f.Hosts, Method: f.Method, Path: f.Path,
		Query: f.Query, Body: f.Body, Status: f.Status, Rewritten: f.Rewritten,
		RespMin: f.RespMin, RespMax: f.RespMax, Sort: "ts", Order: "asc", Page: 0, Size: opts.MaxEntries + 1,
	}
	if q.Size > 500 {
		q.Size = 500
	}
	rows, total, err := t.index.ListExchanges(ctx, q)
	if err != nil {
		return HAR{}, HARExportMeta{}, err
	}
	meta := HARExportMeta{TotalMatched: total}
	if len(rows) > opts.MaxEntries {
		rows = rows[:opts.MaxEntries]
		meta.Truncated = true
		meta.TruncatedReason = "max_entries"
	}
	entries := make([]HAREntry, 0, len(rows))
	for _, row := range rows {
		reqHead, reqBytes, respHead, respBytes, err := t.loadExchangeBodies(row.ID, row.Path)
		if err != nil {
			return HAR{}, HARExportMeta{}, err
		}
		reqProto, reqHeaders := parseStoredRequestHead(reqHead)
		respProto, respStatusText, respHeaders := parseStoredResponseHead(respHead)
		harReq := HARRequest{
			Method: row.Method, URL: redactURLUserinfo(row.URL), HTTPVersion: reqProto,
			Cookies: []HARCookie{}, Headers: reqHeaders, QueryString: []HARParam{}, HeadersSize: -1, BodySize: len(reqBytes),
		}
		if len(reqBytes) > 0 {
			text, enc := encodeHARBody(reqBytes)
			harReq.PostData = &HARPostData{MimeType: findHeaderValue(reqHeaders, "Content-Type"), Text: text, Encoding: enc}
		}
		content := HARContent{Size: len(respBytes), MimeType: row.ContentType}
		if len(respBytes) > 0 {
			content.Text, content.Encoding = encodeHARBody(respBytes)
		}
		entries = append(entries, HAREntry{
			StartedDateTime: time.Unix(row.TS, 0).UTC().Format(time.RFC3339),
			Request:         harReq,
			Response: HARResponse{
				Status: row.Status, StatusText: respStatusText, HTTPVersion: respProto,
				Cookies: []HARCookie{}, Headers: respHeaders, Content: content, HeadersSize: -1, BodySize: len(respBytes),
			},
			Cache: HARCache{}, Timings: HARTimings{Send: -1, Wait: -1, Receive: -1}, ArtexExchangeID: row.ID,
		})
	}
	meta.Exported = len(entries)
	doc := HAR{Log: &HARLog{Version: HARVersion, Creator: HARCreator{Name: "ARTEX", Version: creatorVer}, Entries: entries}}
	if meta.Truncated {
		doc.Log.Truncated = true
		doc.Log.TruncatedReason = meta.TruncatedReason
		doc.Log.TotalMatched = meta.TotalMatched
	}
	return doc, meta, nil
}
