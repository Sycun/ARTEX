package enterprise

import "context"

// migrate 只追加企业岗位和会话里的当前项目。不改已有业务表里的数据。
func (s *Service) migrate(ctx context.Context) error {
	if _, err := s.DB.ExecContext(ctx, `
		ALTER TABLE ent_members ADD COLUMN IF NOT EXISTS post TEXT NOT NULL DEFAULT '';
		ALTER TABLE ent_sessions ADD COLUMN IF NOT EXISTS project_id TEXT`); err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, `
		DO $$
		DECLARE name text;
		BEGIN
		  FOR name IN
		    SELECT con.conname
		    FROM pg_constraint con
		    JOIN pg_class rel ON rel.oid = con.conrelid
		    JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
		    WHERE rel.relname = 'ent_members' AND nsp.nspname = current_schema()
		      AND con.contype = 'c' AND pg_get_constraintdef(con.oid) ILIKE '%role%'
		  LOOP
		    EXECUTE format('ALTER TABLE ent_members DROP CONSTRAINT %I', name);
		  END LOOP;
		  FOR name IN
		    SELECT con.conname
		    FROM pg_constraint con
		    JOIN pg_class rel ON rel.oid = con.conrelid
		    JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
		    WHERE rel.relname = 'ent_duties' AND nsp.nspname = current_schema()
		      AND con.contype = 'c' AND pg_get_constraintdef(con.oid) ILIKE '%duty%'
		  LOOP
		    EXECUTE format('ALTER TABLE ent_duties DROP CONSTRAINT %I', name);
		  END LOOP;
		END $$`); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, `
		ALTER TABLE ent_members ADD CONSTRAINT ent_members_role_check
		  CHECK (role IN ('owner','member','reader','leader'));
		ALTER TABLE ent_duties ADD CONSTRAINT ent_duties_duty_check
		  CHECK (duty IN ('approve','review','audit','lead'))`)
	return err
}
