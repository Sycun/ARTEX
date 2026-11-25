package enterprise

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Autumn-27/artex/notify"
	"github.com/google/uuid"
)

func (s *Service) notify(ctx context.Context, tx *sql.Tx, event string) error {
	snap, ok := notify.EnterpriseSnapshot(event)
	if !ok {
		return fmt.Errorf("unknown enterprise notification %q", event)
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO ent_notifications (id, event_type, summary, outbound_pending)
		VALUES ($1,$2,$3,true)`, uuid.NewString(), event, snap.Summary)
	return err
}

// ForwardNotifications relays committed events. enqueue writes to the delivery
// queue in the relay transaction so a failed relay leaves the events pending.
func (s *Service) ForwardNotifications(ctx context.Context, limit int, enqueue func(context.Context, *sql.Tx, notify.Snapshot) error) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			WITH pending AS (
				SELECT id FROM ent_notifications WHERE outbound_pending
				ORDER BY created_at, id LIMIT $1 FOR UPDATE SKIP LOCKED
			)
			UPDATE ent_notifications n SET outbound_pending=false
			FROM pending WHERE n.id=pending.id RETURNING n.event_type`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		var snaps []notify.Snapshot
		for rows.Next() {
			var event string
			if err := rows.Scan(&event); err != nil {
				return err
			}
			snap, ok := notify.EnterpriseSnapshot(event)
			if !ok {
				return fmt.Errorf("unknown enterprise notification %q", event)
			}
			snaps = append(snaps, snap)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, snap := range snaps {
			if err := enqueue(ctx, tx, snap); err != nil {
				return err
			}
		}
		return nil
	})
}
