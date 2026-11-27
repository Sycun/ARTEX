package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
)

type interceptPendingDTO struct {
	db.InterceptPending
	Risk intercept.RiskReport `json:"risk"`
}

func pendingDTO(p db.InterceptPending) interceptPendingDTO {
	return interceptPendingDTO{InterceptPending: p, Risk: intercept.ScoreRisk(p.ToolName, p.ToolInput)}
}

const maxBatchDecide = 100

type batchDecideResult struct {
	ID    int64  `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (s *Server) interceptBatchDecide(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs      []int64 `json:"ids"`
		Decision string  `json:"decision"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Decision != "allowed" && req.Decision != "denied" {
		writeErr(w, 400, "decision 必须是 allowed 或 denied")
		return
	}
	if len(req.IDs) == 0 || len(req.IDs) > maxBatchDecide {
		writeErr(w, 400, fmt.Sprintf("ids 必须包含 1 到 %d 个待审批编号", maxBatchDecide))
		return
	}
	seen := map[int64]bool{}
	ids := make([]int64, 0, len(req.IDs))
	for _, id := range req.IDs {
		if id <= 0 {
			writeErr(w, 400, fmt.Sprintf("无效审批编号 %d", id))
			return
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	approver, addr := s.approver(r), clientIP(r, s.trustedProxies)
	results := make([]batchDecideResult, 0, len(ids))
	decided := 0
	for _, id := range ids {
		res := batchDecideResult{ID: id, OK: true}
		if err := s.m.interceptor.Decide(id, req.Decision == "allowed", approver, addr); err != nil {
			res.OK, res.Error = false, err.Error()
		} else {
			decided++
		}
		results = append(results, res)
	}
	writeJSON(w, 200, map[string]any{"decided": decided, "failed": len(ids) - decided, "results": results})
}

const alwaysAllowPriority = -1000

func (s *Server) interceptAlwaysAllow(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "bad pending id")
		return
	}
	p, err := pg.GetInterceptPending(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if p == nil {
		writeErr(w, 404, "not found")
		return
	}
	if p.Status != "pending" {
		writeErr(w, 409, "该审批已不在待处理状态")
		return
	}
	prefix, err := intercept.AlwaysAllowPrefix(p.ToolName, p.ToolInput)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	pattern, _ := json.Marshal(prefix)
	rules, err := pg.ListInterceptRules()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, rule := range rules {
		if rule.MatchType != "argv" || rule.Action != "allow" || !rule.Enabled || rule.MatchTarget != "tool_input" {
			continue
		}
		if tokens, err := intercept.ParseArgvPattern(rule.Pattern); err == nil && slices.Equal(tokens, prefix) {
			writeJSON(w, 200, map[string]any{"rule": rule, "created": false})
			return
		}
	}
	rule, err := pg.CreateInterceptRule("always-allow: "+strings.Join(prefix, " "), "tool_input", "argv", string(pattern),
		"allow", "", alwaysAllowPriority, true, true, 60, "deny")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.m.interceptor.Invalidate()
	writeJSON(w, 200, map[string]any{"rule": rule, "created": true})
}

func (s *Server) taskPinScope(w http.ResponseWriter, r *http.Request) (db.TargetScopeRef, bool) {
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "bad task id")
		return db.TargetScopeRef{}, false
	}
	if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
		writeErr(w, 404, "task not found")
		return db.TargetScopeRef{}, false
	}
	return db.TargetScopeRef{TaskID: id}, true
}

func (s *Server) taskListTargetPins(w http.ResponseWriter, r *http.Request) {
	if scope, ok := s.taskPinScope(w, r); ok {
		s.listTargetPins(w, scope)
	}
}

func (s *Server) taskRevokeTargetPin(w http.ResponseWriter, r *http.Request) {
	if scope, ok := s.taskPinScope(w, r); ok {
		s.revokeTargetPin(w, r, scope)
	}
}

func (s *Server) convListTargetPins(w http.ResponseWriter, r *http.Request) {
	if _, c, ok := s.convByID(w, r); ok {
		s.listTargetPins(w, db.TargetScopeRef{ConversationID: c.ID})
	}
}

func (s *Server) convRevokeTargetPin(w http.ResponseWriter, r *http.Request) {
	if _, c, ok := s.convByID(w, r); ok {
		s.revokeTargetPin(w, r, db.TargetScopeRef{ConversationID: c.ID})
	}
}

func (s *Server) listTargetPins(w http.ResponseWriter, scope db.TargetScopeRef) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	pins, err := pg.ListTargetPins(scope)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if pins == nil {
		pins = []*db.TargetPin{}
	}
	writeJSON(w, 200, map[string]any{"pins": pins})
}

func (s *Server) revokeTargetPin(w http.ResponseWriter, r *http.Request, scope db.TargetScopeRef) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	pid, ok := pathInt(r, "pid")
	if !ok {
		writeErr(w, 400, "bad pin id")
		return
	}
	pin, err := pg.RevokeTargetPin(scope, pid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if pin == nil {
		writeErr(w, 404, "pin not found")
		return
	}
	writeJSON(w, 200, pin)
}

func (s *Server) targetGateGetExemptions(w http.ResponseWriter, r *http.Request) {
	list, digest, err := s.m.interceptor.Exemptions()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if list == nil {
		list = []db.TargetExemption{}
	}
	writeJSON(w, 200, map[string]any{"exemptions": list, "digest": digest, "derived": s.m.interceptor.DerivedExemptions()})
}

func (s *Server) targetGateSetExemptions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Exemptions []db.TargetExemption `json:"exemptions"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	list, digest, err := s.m.interceptor.SetExemptions(req.Exemptions, s.approver(r))
	if err != nil {
		if errors.Is(err, intercept.ErrInvalidExemption) {
			writeErr(w, 400, err.Error())
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"exemptions": list, "digest": digest})
}

func (s *Server) targetGateExemptionHistory(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	changes, err := pg.ListTargetExemptionLog(100)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"changes": changes})
}
