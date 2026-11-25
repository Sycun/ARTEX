package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Autumn-27/artex/db"
)

const (
	maxTaskNameRunes           = 200
	maxTaskMetadataRequestSize = 16 << 10
)

// updateTaskMetadata updates editable metadata and the live asset-sharing policy.
func (s *Server) updateTaskMetadata(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	if _, ok := s.m.Task(taskID); !ok {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if actor, ok := currentActor(r); ok {
		id, _ := strconv.ParseInt(taskID, 10, 64)
		if _, err := s.ent.TaskAccess(r.Context(), actor, id, true); err != nil {
			writeEnt(w, err)
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTaskMetadataRequestSize)
	var request struct {
		Name              *string `json:"name"`
		Remark            *string `json:"remark"`
		Pinned            *bool   `json:"pinned"`
		ShareGlobalAssets *bool   `json:"share_global_assets"`
		Zone              *string `json:"zone"`
	}
	if err := decode(r, &request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "请求正文过大")
		} else {
			writeErr(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	if _, ok := currentActor(r); ok && request.ShareGlobalAssets != nil {
		writeProductError(w, http.StatusForbidden, "read_only", "企业会话不能改全局资产共享", nil)
		return
	}
	if request.Name == nil && request.Pinned == nil && request.Remark == nil && request.ShareGlobalAssets == nil && request.Zone == nil {
		writeErr(w, http.StatusBadRequest, "至少需要提供 name、pinned、remark、share_global_assets 或 zone")
		return
	}
	if request.Name != nil {
		name := strings.TrimSpace(*request.Name)
		if name == "" {
			writeErr(w, http.StatusBadRequest, "任务名称不能为空")
			return
		}
		if utf8.RuneCountInString(name) > maxTaskNameRunes {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("任务名称最多 %d 个字符", maxTaskNameRunes))
			return
		}
		request.Name = &name
	}

	if request.Remark != nil {
		remark, err := normalizeRemark(*request.Remark)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		request.Remark = &remark
	}
	if request.Zone != nil {
		zone, err := db.NormalizeZone(*request.Zone)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		request.Zone = &zone
	}
	task, err := s.m.UpdateTaskMetadata(taskID, db.TaskPatch{Name: request.Name, Pinned: request.Pinned, Remark: request.Remark, ShareGlobalAssets: request.ShareGlobalAssets, Zone: request.Zone})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if task == nil {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if request.Zone != nil {
		id, convErr := strconv.ParseInt(task.ID, 10, 64)
		if convErr != nil {
			writeErr(w, http.StatusBadRequest, "任务 id 无效")
			return
		}
		if err := s.m.pg.RetargetExecutionJobZone(r.Context(), id, *request.Zone); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, taskDTO(task, s.resolvedTaskStatus(task)))
}
