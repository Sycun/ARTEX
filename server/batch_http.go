package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Autumn-27/artex/db"
)

func (s *Server) createBatch(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		ProjectID string   `json:"project_id"`
		Kind      string   `json:"kind"`
		Zone      string   `json:"zone"`
		ShardSize int      `json:"shard_size"`
		Targets   []string `json:"targets"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	req.Kind = strings.TrimSpace(req.Kind)
	switch req.Kind {
	case db.BatchKindAssetRecon, db.BatchKindFindingRetest:
	default:
		writeErr(w, http.StatusBadRequest, "kind must be asset_recon or finding_retest")
		return
	}
	req.ProjectID = strings.TrimSpace(req.ProjectID)
	if !s.authorizeBatch(w, r, req.ProjectID, true) {
		return
	}
	job, err := pg.CreateBatchJob(r.Context(), db.BatchCreate{
		ProjectID: req.ProjectID,
		Kind:      req.Kind,
		Zone:      req.Zone,
		ShardSize: req.ShardSize,
		Targets:   req.Targets,
		CreatedBy: batchActorID(r),
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"batch": job})
}

func (s *Server) getBatch(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	job, err := pg.GetBatchJob(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "batch not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !s.authorizeBatch(w, r, job.ProjectID, false) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch": job})
}

func (s *Server) cancelBatch(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	current, err := pg.GetBatchJob(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "batch not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !s.authorizeBatch(w, r, current.ProjectID, true) {
		return
	}
	job, err := pg.CancelBatchJob(r.Context(), current.ID)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "batch not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch": job})
}

func (s *Server) authorizeBatch(w http.ResponseWriter, r *http.Request, projectID string, write bool) bool {
	if !s.entMode {
		return true
	}
	actor, ok := s.wantEnt(w, r)
	if !ok {
		return false
	}
	if err := s.ent.AuthorizeBatch(r.Context(), actor, projectID, write); err != nil {
		writeEnt(w, err)
		return false
	}
	return true
}

func batchActorID(r *http.Request) string {
	if actor, ok := currentActor(r); ok {
		return actor.ID
	}
	return ""
}
