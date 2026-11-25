package server

import (
	"log"
	"sync"

	"github.com/Autumn-27/artex/db"
)

type ControlEventKind string

const (
	ControlTaskPause  ControlEventKind = "task_pause"
	ControlTaskResume ControlEventKind = "task_resume"
	ControlTaskCancel ControlEventKind = "task_cancel"
	ControlTaskDelete ControlEventKind = "task_delete"
)

type ControlEvent struct {
	Kind   ControlEventKind `json:"kind"`
	TaskID string           `json:"task_id"`
}

// EventBus carries best-effort live notifications. Activity and task state are
// persisted separately. Callers must unsubscribe when they stop consuming.
type EventBus interface {
	SubscribeActivity(task string) (<-chan db.Activity, func())
	PublishActivity(task string, activity db.Activity) error
	SubscribeControl() (<-chan ControlEvent, func())
	PublishControl(event ControlEvent) error
}

// MemoryBus fans out events without I/O or waiting for slow subscribers.
type MemoryBus struct {
	activity subscriptions[string, db.Activity]
	control  subscriptions[struct{}, ControlEvent]
}

func (b *MemoryBus) SubscribeActivity(task string) (<-chan db.Activity, func()) {
	return b.activity.subscribe(task)
}

func (b *MemoryBus) PublishActivity(task string, activity db.Activity) error {
	b.activity.publish(task, activity)
	return nil
}

func (b *MemoryBus) SubscribeControl() (<-chan ControlEvent, func()) {
	return b.control.subscribe(struct{}{})
}

func (b *MemoryBus) PublishControl(event ControlEvent) error {
	b.control.publish(struct{}{}, event)
	return nil
}

type subscriptions[K comparable, V any] struct {
	mu   sync.Mutex
	subs map[K]map[chan V]struct{}
}

func (s *subscriptions[K, V]) subscribe(key K) (<-chan V, func()) {
	ch := make(chan V, 256)
	s.mu.Lock()
	if s.subs == nil {
		s.subs = make(map[K]map[chan V]struct{})
	}
	if s.subs[key] == nil {
		s.subs[key] = make(map[chan V]struct{})
	}
	s.subs[key][ch] = struct{}{}
	s.mu.Unlock()

	return ch, sync.OnceFunc(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.subs[key], ch)
		if len(s.subs[key]) == 0 {
			delete(s.subs, key)
		}
		close(ch)
	})
}

func (s *subscriptions[K, V]) publish(key K, value V) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs[key] {
		select {
		case ch <- value:
		default: // Activity consumers can replay from their last persisted cursor.
		}
	}
}

func (s *subscriptions[K, V]) has(key K) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs[key]) != 0
}

// Broadcaster preserves the engine/SSE API while delegating fan-out to the bus.
type Broadcaster struct{ bus EventBus }

func NewBroadcaster() *Broadcaster { return &Broadcaster{bus: &MemoryBus{}} }

func (b *Broadcaster) Subscribe(task string) (<-chan db.Activity, func()) {
	return b.bus.SubscribeActivity(task)
}

func (b *Broadcaster) Publish(task string, a db.Activity) {
	if err := b.bus.PublishActivity(task, a); err != nil {
		log.Printf("[event bus] publish activity task=%q id=%d: %v", task, a.ID, err)
	}
}
