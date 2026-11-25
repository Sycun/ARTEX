package traffic

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/storage"
)

// IndexStore is the exchange index. SQLiteStore is the single-process backend.
// PGStore keeps metadata in PostgreSQL and bodies in a BlobStore.
type IndexStore interface {
	InsertExchange(ctx context.Context, ex ExchangeWrite) error
	ListExchanges(ctx context.Context, q ListQuery) ([]ExchangeMeta, int, error)
	GetExchange(ctx context.Context, id string) (ExchangeRead, error)
	HostStats(ctx context.Context, scope []string) ([]HostCount, error)
	TemplateStats(ctx context.Context, host string) ([]TemplateStat, error)
	DeleteHost(ctx context.Context, host string) (int64, error)
	DeleteHosts(ctx context.Context, hosts []string) (int64, error)
	DeleteExchange(ctx context.Context, id string) error
	DeleteAll(ctx context.Context) (int64, error)
	BlobRefCount(ctx context.Context, blobKey string) (int, error)
}

// ExchangeWrite is one captured exchange. Empty bodies are not stored as blobs.
type ExchangeWrite struct {
	ID          string
	TaskID      string
	ProjectID   string
	TS          time.Time
	Host        string
	Method      string
	URLTemplate string
	URL         string
	Status      int
	ContentType string
	ReqLen      int
	RespLen     int
	ReqHead     string
	RespHead    string
	ReqBody     []byte
	RespBody    []byte
}

// ExchangeRead is metadata plus the raw bodies.
type ExchangeRead struct {
	ExchangeMeta
	TaskID      string
	ProjectID   string
	ReqLen      int
	ReqHead     string
	RespHead    string
	ReqBody     []byte
	RespBody    []byte
	ReqBlobKey  string
	RespBlobKey string
}

func (e ExchangeRead) RequestText() string  { return e.ReqHead + "\n" + string(e.ReqBody) }
func (e ExchangeRead) ResponseText() string { return e.RespHead + "\n" + string(e.RespBody) }

// TemplateStat is one host + URL template aggregate.
type TemplateStat struct {
	Host        string `json:"host"`
	URLTemplate string `json:"url_template"`
	Count       int    `json:"count"`
}

// ListQuery filters a page of exchanges. Negative RespMin/RespMax are unset.
// ExactHosts nil means "no exact-host constraint"; an empty slice matches nothing.
type ListQuery struct {
	Host       string
	HostLike   bool
	Hosts      []string
	ExactHosts []string
	Method     string
	Path       string
	Contains   string
	Authority  string
	Query      string
	Body       string
	Status     string
	RespMin    int64
	RespMax    int64
	TaskID     string
	ProjectID  string
	Sort       string
	Order      string
	Page       int
	Size       int
}

type exchangeCols struct {
	host, method, url, template, status, resp, content, id string
	reqHead, respHead, task, project, ts                   string
}

func pgExchangeCols() exchangeCols {
	return exchangeCols{
		host: "host", method: "method", url: "url", template: "url_template",
		status: "status", resp: "resp_len", content: "content_type", id: "id",
		reqHead: "req_head", respHead: "resp_head", task: "task_id", project: "project_id",
		ts: "ts",
	}
}

func sqliteExchangeCols() exchangeCols {
	return exchangeCols{
		host: "e.host", method: "e.method", url: "e.url", template: "e.url_template",
		status: "e.status", resp: "e.resp_len", content: "e.content_type", id: "e.id",
		reqHead: "b.req_head", respHead: "b.resp_head", ts: "e.ts",
	}
}

type sqlArgs struct {
	vals []any
	pg   bool
	n    int
}

func (a *sqlArgs) ph(v any) string {
	a.vals = append(a.vals, v)
	if !a.pg {
		return "?"
	}
	a.n++
	return fmt.Sprintf("$%d", a.n)
}

func (a *sqlArgs) like() string {
	if a.pg {
		return "ILIKE"
	}
	return "LIKE"
}

func normalizePage(page, size int) (int, int) {
	if size <= 0 || size > 500 {
		size = 100
	}
	if page < 0 {
		page = 0
	}
	return page, size
}

func whereSQL(q ListQuery, a *sqlArgs, c exchangeCols) (string, error) {
	if (q.TaskID != "" || q.ProjectID != "") && c.task == "" {
		return "", fmt.Errorf("sqlite traffic index does not store task_id")
	}
	var parts []string
	add := func(s string) { parts = append(parts, s) }
	like := a.like()
	if h := strings.TrimSpace(q.Host); h != "" {
		if q.HostLike {
			add(c.host + " " + like + " " + a.ph("%"+h+"%"))
		} else {
			add(c.host + " = " + a.ph(h))
		}
	}
	if q.ExactHosts != nil {
		if len(q.ExactHosts) == 0 {
			add("false")
		} else {
			ors := make([]string, 0, len(q.ExactHosts))
			for _, h := range q.ExactHosts {
				ors = append(ors, c.host+" = "+a.ph(h))
			}
			add("(" + strings.Join(ors, " OR ") + ")")
		}
	}
	if scope := hostScopeSQL(q.Hosts, c.host, like, a.ph); scope != "" {
		add(scope)
	}
	if m := strings.TrimSpace(q.Method); m != "" {
		add("upper(" + c.method + ") = " + a.ph(strings.ToUpper(m)))
	}
	if p := strings.TrimSpace(q.Path); p != "" {
		add(c.template + " " + like + " " + a.ph("%"+p+"%"))
	}
	if s := strings.TrimSpace(q.Contains); s != "" {
		add("(" + c.url + " " + like + " " + a.ph("%"+s+"%") + " OR " + c.template + " " + like + " " + a.ph("%"+s+"%") + ")")
	}
	if auth := strings.TrimSpace(q.Authority); auth != "" {
		add("(" + c.url + " " + like + " " + a.ph("%://"+auth+"/%") +
			" OR " + c.url + " " + like + " " + a.ph("%://"+auth+"?%") +
			" OR " + c.url + " " + like + " " + a.ph("%://"+auth) + ")")
	}
	if cond, ok := statusSQL(q.Status, c.status, a.ph); ok {
		add(cond)
	}
	if q.RespMin >= 0 {
		add(c.resp + " >= " + a.ph(q.RespMin))
	}
	if q.RespMax >= 0 {
		add(c.resp + " <= " + a.ph(q.RespMax))
	}
	if q.TaskID != "" {
		add(c.task + " = " + a.ph(q.TaskID))
	}
	if q.ProjectID != "" {
		add(c.project + " = " + a.ph(q.ProjectID))
	}
	if s := strings.TrimSpace(q.Query); s != "" {
		meta := []string{
			c.id + " " + like + " " + a.ph("%"+s+"%"),
			c.host + " " + like + " " + a.ph("%"+s+"%"),
			c.url + " " + like + " " + a.ph("%"+s+"%"),
			c.template + " " + like + " " + a.ph("%"+s+"%"),
			c.method + " " + like + " " + a.ph("%"+s+"%"),
			c.content + " " + like + " " + a.ph("%"+s+"%"),
			"CAST(" + c.status + " AS TEXT) " + like + " " + a.ph("%"+s+"%"),
		}
		if c.reqHead != "" {
			meta = append(meta,
				c.reqHead+" "+like+" "+a.ph("%"+s+"%"),
				c.respHead+" "+like+" "+a.ph("%"+s+"%"))
		}
		add("(" + strings.Join(meta, " OR ") + ")")
	}
	if b := strings.TrimSpace(q.Body); b != "" && c.reqHead != "" {
		add("(" + c.reqHead + " " + like + " " + a.ph("%"+b+"%") + " OR " + c.respHead + " " + like + " " + a.ph("%"+b+"%") + ")")
	}
	if len(parts) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(parts, " AND "), nil
}

func hostScopeSQL(hosts []string, hostCol, like string, ph func(any) string) string {
	var conds []string
	seen := map[string]struct{}{}
	for _, raw := range hosts {
		host := strings.ToLower(strings.TrimSpace(raw))
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		if strings.HasPrefix(host, "*.") {
			base := strings.TrimPrefix(host, "*.")
			if base == "" {
				continue
			}
			conds = append(conds, "("+hostCol+" = "+ph(base)+" OR "+hostCol+" "+like+" "+ph(base+":%")+" OR "+hostCol+" "+like+" "+ph("%."+base)+")")
			continue
		}
		conds = append(conds, "("+hostCol+" = "+ph(host)+" OR "+hostCol+" "+like+" "+ph(host+":%")+")")
	}
	if len(conds) == 0 {
		return ""
	}
	return "(" + strings.Join(conds, " OR ") + ")"
}

func statusSQL(status, col string, ph func(any) string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(status))
	if s == "" {
		return "", false
	}
	if len(s) == 3 && s[0] >= '1' && s[0] <= '5' && s[1] == 'x' && s[2] == 'x' {
		base := int(s[0]-'0') * 100
		return col + " >= " + ph(base) + " AND " + col + " < " + ph(base+100), true
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return "", false
	}
	return col + " = " + ph(n), true
}

func orderSQL(sort, order string, c exchangeCols) string {
	col := c.ts
	switch strings.ToLower(strings.TrimSpace(sort)) {
	case "status":
		col = c.status
	case "resp_len":
		col = c.resp
	}
	dir := "DESC"
	if strings.EqualFold(strings.TrimSpace(order), "asc") {
		dir = "ASC"
	}
	return col + " " + dir + ", " + c.id + " DESC"
}

// Backend resolves sqlite or pg. ARTEX_TRAFFIC_STORE wins. api and worker
// default to pg; all and the empty mode default to sqlite.
func Backend(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ARTEX_TRAFFIC_STORE"))) {
	case "":
		if mode == "api" || mode == "worker" {
			return "pg", nil
		}
		return "sqlite", nil
	case "sqlite":
		return "sqlite", nil
	case "pg":
		return "pg", nil
	default:
		return "", fmt.Errorf("ARTEX_TRAFFIC_STORE must be sqlite or pg")
	}
}

// OpenBlobStore returns the configured S3 bucket, or a local directory.
func OpenBlobStore(dir string) (storage.BlobStore, error) {
	cfg := storage.S3Config{
		Endpoint:  os.Getenv("ARTEX_S3_ENDPOINT"),
		Bucket:    os.Getenv("ARTEX_S3_BUCKET"),
		Region:    os.Getenv("ARTEX_S3_REGION"),
		AccessKey: os.Getenv("ARTEX_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("ARTEX_S3_SECRET_KEY"),
	}
	if cfg != (storage.S3Config{}) {
		return storage.NewS3Store(cfg)
	}
	return storage.NewLocalFSStore(dir)
}
