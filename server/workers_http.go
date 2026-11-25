package server

import (
	"net/http"

	"github.com/Autumn-27/artex/db"
)

func (s *Server) listWorkers(w http.ResponseWriter, r *http.Request) {
	if s.entMode {
		actor, ok := currentActor(r)
		if !ok || !actor.Platform() {
			writeProductError(w, http.StatusForbidden, "forbidden", "仅平台管理员可访问", nil)
			return
		}
	}
	pg := s.pg(w)
	if pg == nil {
		return
	}
	workers, err := pg.ListWorkerNodes(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if workers == nil {
		workers = []db.WorkerNode{}
	}
	zones, err := pg.ExecutionZoneStats(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if zones == nil {
		zones = []db.ZoneJobStats{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"workers": workers, "zones": zones})
}
