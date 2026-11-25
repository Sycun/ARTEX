package server

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

func (s *Server) enqueueTaskExecution(t *Task) error {
	if t == nil || s.m == nil || s.m.pg == nil {
		return fmt.Errorf("execution queue is unavailable")
	}
	taskID, err := strconv.ParseInt(t.ID, 10, 64)
	if err != nil || taskID <= 0 {
		return fmt.Errorf("task id %q is invalid", t.ID)
	}
	row, err := s.m.pg.GetTask(taskID)
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("task %d not found", taskID)
	}
	var project sql.NullString
	err = s.m.pg.QueryRowContext(s.ctx, `SELECT project_id FROM ent_project_tasks WHERE task_id=$1 LIMIT 1`, taskID).Scan(&project)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := s.m.pg.EnqueueExecutionJob(s.ctx, taskID, project.String, row.Zone); err != nil {
		return err
	}
	return nil
}

func (s *Server) bindExecutionControl(kind ControlEventKind, taskID string) error {
	if s.mode != ModeAPI || s.m == nil || s.m.pg == nil {
		return nil
	}
	id, err := strconv.ParseInt(taskID, 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("task id %q is invalid", taskID)
	}
	switch kind {
	case ControlTaskPause:
		if err := s.m.pg.PauseExecutionJobByTask(s.ctx, id); err != nil {
			return err
		}
	case ControlTaskCancel, ControlTaskDelete:
		if err := s.m.pg.CancelExecutionJobByTask(s.ctx, id); err != nil {
			return err
		}
	}
	bus := s.EventBus()
	if bus == nil {
		return fmt.Errorf("event bus is not configured")
	}
	return bus.PublishControl(ControlEvent{Kind: kind, TaskID: taskID})
}
