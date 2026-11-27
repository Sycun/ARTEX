//go:build embedui

package server

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// webuiDist holds the statically-exported frontend (built with
// `cd web && npm run build:static`, then copied into server/webui/dist). Compiled
// into the binary only when building with `-tags embedui`. The `all:` prefix is
// required so Next's `_next/` asset dir (leading underscore) is included.
//
//go:embed all:webui/dist
var webuiDist embed.FS

// webuiHandler serves the embedded SPA. Public (no JWT) — auth is enforced
// client-side and on /api. Serves the exported per-route index.html files and
// falls back to index.html so client-side routing still resolves unknown paths.
func (s *Server) webuiHandler() http.Handler {
	root, err := fs.Sub(webuiDist, "webui/dist")
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		// 1) exact file (assets: _next/*, favicon.ico, ...)
		// 2) route dir → <p>/index.html (trailingSlash export)
		// 3) <p>.html
		// 4) dynamic static-export shell (e.g. enterprise/p/_/tasks/_/index.html)
		// 5) SPA 兜底 → index.html（交给客户端路由）
		for _, cand := range []string{p, p + "/index.html", p + ".html"} {
			if serveFileIfExists(w, r, root, cand) {
				return
			}
		}
		if shell := dynamicExportShell(p); shell != "" && serveFileIfExists(w, r, root, shell) {
			return
		}
		http.ServeFileFS(w, r, root, "index.html")
	})
}

func dynamicExportShell(p string) string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) == 3 && parts[0] == "enterprise" && parts[1] == "conclusions" {
		return "enterprise/conclusions/_/index.html"
	}
	if len(parts) >= 3 && parts[0] == "enterprise" && parts[1] == "p" {
		parts[2] = "_"
		if len(parts) == 5 && (parts[3] == "findings" || parts[3] == "reports" || parts[3] == "tasks") {
			parts[4] = "_"
		}
		return strings.Join(parts, "/") + "/index.html"
	}
	return ""
}

// serveFileIfExists serves name from fsys when it exists as a regular file.
func serveFileIfExists(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) bool {
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	st, statErr := f.Stat()
	_ = f.Close()
	if statErr != nil || st.IsDir() {
		return false
	}
	http.ServeFileFS(w, r, fsys, name)
	return true
}
