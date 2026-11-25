package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	pgActivityChannel = "artex_activity"
	pgControlChannel  = "artex_control"
	pgNotifyMaxBytes  = 7 * 1024
	pgNotifyTimeout   = 3 * time.Second
)

func configuredEventBus(ctx context.Context, mode, dsn string, pg *db.DB) (EventBus, error) {
	switch mode {
	case "", "memory":
		return &MemoryBus{}, nil
	case "pg":
		return NewPGNotifyBus(ctx, dsn, pg)
	default:
		return nil, fmt.Errorf("unknown ARTEX_EVENT_BUS %q (expected pg or memory)", mode)
	}
}

type PGNotifyBus struct {
	local  MemoryBus
	pg     *db.DB
	origin uuid.UUID
	ctx    context.Context
	done   chan struct{}
}

type pgActivityEvent struct {
	Origin     uuid.UUID    `json:"origin"`
	TaskID     string       `json:"task_id"`
	Activity   *db.Activity `json:"activity,omitempty"`
	ActivityID int64        `json:"activity_id,omitempty"`
}

type pgControlEvent struct {
	Origin uuid.UUID `json:"origin"`
	ControlEvent
}

// NewPGNotifyBus registers LISTEN before returning. The listener owns an
// unpooled pgx session and stops when ctx is canceled.
func NewPGNotifyBus(ctx context.Context, dsn string, pg *db.DB) (*PGNotifyBus, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	conn, err := openNotifyListener(ctx, cfg)
	if err != nil {
		return nil, err
	}
	b := &PGNotifyBus{pg: pg, origin: uuid.New(), ctx: ctx, done: make(chan struct{})}
	go b.listen(cfg, conn)
	return b, nil
}

func openNotifyListener(ctx context.Context, cfg *pgx.ConnConfig) (*pgx.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, pgNotifyTimeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, "LISTEN "+pgActivityChannel+"; LISTEN "+pgControlChannel); err != nil {
		closeNotifyListener(conn)
		return nil, err
	}
	return conn, nil
}

func closeNotifyListener(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), pgNotifyTimeout)
	defer cancel()
	if err := conn.Close(ctx); err != nil {
		log.Printf("[event bus] close listener: %v", err)
	}
}

func (b *PGNotifyBus) listen(cfg *pgx.ConnConfig, conn *pgx.Conn) {
	defer close(b.done)
	for {
		for {
			n, err := conn.WaitForNotification(b.ctx)
			if err != nil {
				if b.ctx.Err() == nil {
					log.Printf("[event bus] listener disconnected: %v", err)
				}
				break
			}
			b.receive(n.Channel, n.Payload)
		}
		closeNotifyListener(conn)

		retry := time.NewTicker(time.Second)
		for {
			select {
			case <-b.ctx.Done():
				retry.Stop()
				return
			case <-retry.C:
			}
			var err error
			conn, err = openNotifyListener(b.ctx, cfg)
			if err == nil {
				retry.Stop()
				break
			}
			if b.ctx.Err() == nil {
				log.Printf("[event bus] reconnect listener: %v", err)
			}
		}
	}
}

func (b *PGNotifyBus) SubscribeActivity(task string) (<-chan db.Activity, func()) {
	return b.local.SubscribeActivity(task)
}

func (b *PGNotifyBus) PublishActivity(task string, a db.Activity) error {
	b.local.PublishActivity(task, a)
	// Persisted activities use references even when small. NOTIFY is visible to
	// other database users and should not contain task transcripts or metadata.
	wire := pgActivityEvent{Origin: b.origin, TaskID: task, ActivityID: a.ID}
	if a.ID == 0 {
		wire.Activity = &a
	}
	payload, err := json.Marshal(wire)
	if err != nil {
		return err
	}
	if len(payload) > pgNotifyMaxBytes && wire.Activity != nil {
		wire.Activity = &db.Activity{NodeID: a.NodeID, IsError: a.IsError, CreatedAt: a.CreatedAt}
		payload, err = json.Marshal(wire)
		if err != nil {
			return err
		}
	}
	return b.notify(pgActivityChannel, payload)
}

func (b *PGNotifyBus) SubscribeControl() (<-chan ControlEvent, func()) {
	return b.local.SubscribeControl()
}

func (b *PGNotifyBus) PublishControl(event ControlEvent) error {
	payload, err := json.Marshal(pgControlEvent{Origin: b.origin, ControlEvent: event})
	if err != nil {
		return err
	}
	if err := b.notify(pgControlChannel, payload); err != nil {
		return err
	}
	return b.local.PublishControl(event)
}

func (b *PGNotifyBus) notify(channel string, payload []byte) error {
	if len(payload) > pgNotifyMaxBytes {
		return fmt.Errorf("notification routing data exceeds %d bytes", pgNotifyMaxBytes)
	}
	ctx, cancel := context.WithTimeout(b.ctx, pgNotifyTimeout)
	defer cancel()
	_, err := b.pg.ExecContext(ctx, "SELECT pg_notify($1, $2)", channel, string(payload))
	return err
}

func (b *PGNotifyBus) receive(channel, payload string) {
	switch channel {
	case pgActivityChannel:
		var event pgActivityEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			log.Printf("[event bus] decode activity: %v", err)
			return
		}
		if event.Origin == b.origin || !b.local.activity.has(event.TaskID) {
			return
		}
		if event.Activity != nil {
			b.local.PublishActivity(event.TaskID, *event.Activity)
			return
		}
		id, err := strconv.ParseInt(event.TaskID, 10, 64)
		if err != nil || id <= 0 {
			log.Printf("[event bus] invalid activity task ID %q", event.TaskID)
			return
		}
		ctx, cancel := context.WithTimeout(b.ctx, pgNotifyTimeout)
		defer cancel()
		a, err := b.pg.TaskActivity(ctx, id, event.ActivityID)
		if errors.Is(err, sql.ErrNoRows) {
			return // The activity or task may have been deleted after publish.
		}
		if err != nil {
			log.Printf("[event bus] load activity task=%q id=%d: %v", event.TaskID, event.ActivityID, err)
			return
		}
		b.local.PublishActivity(event.TaskID, a)
	case pgControlChannel:
		var event pgControlEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			log.Printf("[event bus] decode control: %v", err)
			return
		}
		if event.Origin != b.origin {
			b.local.PublishControl(event.ControlEvent)
		}
	}
}
