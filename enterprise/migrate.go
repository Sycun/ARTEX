package enterprise

import "context"

// migrate 更新企业版表结构并保留存量业务数据。
func (s *Service) migrate(ctx context.Context) error {
	if _, err := s.DB.ExecContext(ctx, `
		ALTER TABLE ent_members ADD COLUMN IF NOT EXISTS post TEXT NOT NULL DEFAULT '';
		ALTER TABLE ent_sessions ADD COLUMN IF NOT EXISTS project_id TEXT;
		ALTER TABLE ent_issues ADD COLUMN IF NOT EXISTS blob_key TEXT;
		ALTER TABLE ent_issues ALTER COLUMN file_bytes DROP NOT NULL;
		ALTER TABLE ent_notifications ADD COLUMN IF NOT EXISTS outbound_pending BOOLEAN NOT NULL DEFAULT false;
		CREATE INDEX IF NOT EXISTS idx_ent_notifications_outbound_pending
		  ON ent_notifications(created_at, id) WHERE outbound_pending`); err != nil {
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
