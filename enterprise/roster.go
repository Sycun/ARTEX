package enterprise

import (
	"context"
	"database/sql"
)

// EnsureWorkRoster 给本地演示身份源准备员工、复核人和领导。
// 已有成员不降级：项目负责人保持原角色，只补还没有的岗位。
func (s *Service) EnsureWorkRoster(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		projectA, err := s.ensureProject(ctx, tx, ProjectA, "门户整改")
		if err != nil {
			return err
		}
		projectB, err := s.ensureProject(ctx, tx, ProjectB, "季度巡检")
		if err != nil {
			return err
		}
		if err := s.ensureOwner(ctx, tx, projectA, UserOwnerA, UserBiz); err != nil {
			return err
		}
		if err := s.ensureOwner(ctx, tx, projectB, UserOwnerB, UserBiz); err != nil {
			return err
		}
		if err := s.ensureSeat(ctx, tx, projectA, UserAnalyst, "member", "employee", nil); err != nil {
			return err
		}
		if err := s.ensureSeat(ctx, tx, projectA, UserReviewer, "member", "reviewer", []string{"review"}); err != nil {
			return err
		}
		if err := s.ensureSeat(ctx, tx, projectA, UserBiz, "leader", "leader", []string{"lead"}); err != nil {
			return err
		}
		return s.ensureSeat(ctx, tx, projectB, UserMemberB, "member", "employee", nil)
	})
}

func (s *Service) ensureSeat(ctx context.Context, tx *sql.Tx, projectID, userID, role, post string, duties []string) error {
	var existingRole, existingPost string
	err := tx.QueryRowContext(ctx, `
		SELECT role, post FROM ent_members WHERE project_id=$1 AND user_id=$2`, projectID, userID).
		Scan(&existingRole, &existingPost)
	switch {
	case err == sql.ErrNoRows:
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_members (project_id, user_id, role, post) VALUES ($1,$2,$3,$4)`,
			projectID, userID, role, post); err != nil {
			return err
		}
	case err != nil:
		return err
	case existingRole == "owner":
		if existingPost == "" {
			if _, err := tx.ExecContext(ctx, `
				UPDATE ent_members SET post='employee' WHERE project_id=$1 AND user_id=$2 AND post=''`,
				projectID, userID); err != nil {
				return err
			}
		}
	default:
		if _, err := tx.ExecContext(ctx, `
			UPDATE ent_members SET role=$3, post=$4 WHERE project_id=$1 AND user_id=$2`,
			projectID, userID, role, post); err != nil {
			return err
		}
	}
	for _, duty := range duties {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ent_duties (id, project_id, user_id, duty, granted_by, grant_ref)
			VALUES ($1,$2,$3,$4,$5,'work-roster')
			ON CONFLICT (project_id, user_id, duty) DO NOTHING`,
			newID(), projectID, userID, duty, UserBiz); err != nil {
			return err
		}
	}
	return nil
}
