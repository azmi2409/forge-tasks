// Package bus is forge-tasks' in-process event bus (component 6): a
// non-blocking pub/sub with cursor-based replay for the SSE stream.
//
// Design for high load:
//   - Publishers never block: each subscriber has a bounded buffer, and a
//     subscriber that falls behind is disconnected (it reconnects with its
//     cursor and replays what it missed).
//   - A bounded ring retains the last ringSize events for replay; cursors
//     older than the ring are rejected so clients resync explicitly
//     instead of silently missing history.
package bus

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	// ringSize bounds replay history.
	ringSize = 1024
	// subBuffer bounds each live subscriber's queue.
	subBuffer = 128
)

// ErrCursorTooOld means the requested cursor fell out of the replay ring.
// The client must resync (full list) instead of streaming.
var ErrCursorTooOld = errors.New("bus: cursor too old, resync required")

// Event is one bus message. Seq is the global cursor.
type Event struct {
	Seq  uint64    `json:"seq"`
	Type string    `json:"type"`
	At   time.Time `json:"at"`
	Data any       `json:"data,omitempty"`
}

// Event types published by the store and service layers.
const (
	TypeProjectCreated     = "project.created"
	TypeWorkItemCreated    = "work_item.created"
	TypeWorkItemUpdated    = "work_item.updated"
	TypeWorkItemDeleted    = "work_item.deleted"
	TypeClaimCreated       = "claim.created"
	TypeClaimRenewed       = "claim.renewed"
	TypeClaimReleased      = "claim.released"
	TypeWorkflowCreated    = "workflow.created"
	TypeWorkflowUpdated    = "workflow.updated"
	TypeProjectWorkflowSet = "project.workflow_pinned"
	TypeEvalRunRecorded    = "eval_run.recorded"
	TypeTransitionApplied  = "transition.applied"
	TypeArtifactCreated    = "artifact.created"
)

// Bus is a non-blocking pub/sub with replay.
type Bus struct {
	mu   sync.Mutex
	seq  uint64
	ring []Event
	subs map[chan Event]struct{}
}

// New returns an empty Bus.
func New() *Bus {
	return &Bus{subs: make(map[chan Event]struct{})}
}

// CurrentSeq returns the latest assigned sequence number.
func (b *Bus) CurrentSeq() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

// Publish assigns the next sequence number, retains the event for replay,
// and fans out to subscribers without blocking. Slow subscribers are
// disconnected; they reconnect with their cursor to replay.
func (b *Bus) Publish(typ string, data any) Event {
	b.mu.Lock()
	b.seq++
	ev := Event{Seq: b.seq, Type: typ, At: time.Now().UTC(), Data: data}
	b.ring = append(b.ring, ev)
	if len(b.ring) > 2*ringSize {
		b.ring = append([]Event(nil), b.ring[len(b.ring)-ringSize:]...)
	}
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
			close(ch)
			delete(b.subs, ch)
		}
	}
	b.mu.Unlock()
	return ev
}

// Subscribe returns a channel of events starting after fromSeq.
// fromSeq == 0 means live-only (no replay). A fromSeq older than the
// retained ring returns ErrCursorTooOld. The channel is closed when the
// subscriber falls behind or ctx is done; the caller must always drain it.
func (b *Bus) Subscribe(ctx context.Context, fromSeq uint64) (<-chan Event, error) {
	b.mu.Lock()
	var replay []Event
	if fromSeq > 0 {
		if len(b.ring) > 0 && fromSeq < b.ring[0].Seq {
			b.mu.Unlock()
			return nil, ErrCursorTooOld
		}
		if fromSeq <= b.seq {
			for _, ev := range b.ring {
				if ev.Seq >= fromSeq {
					replay = append(replay, ev)
				}
			}
		}
		// fromSeq > b.seq: future cursor, live-only from here.
	}
	ch := make(chan Event, max2(subBuffer, len(replay)))
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	for _, ev := range replay {
		ch <- ev // fits: buffer was sized for it
	}
	go func() {
		<-ctx.Done()
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}()
	return ch, nil
}

func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}
