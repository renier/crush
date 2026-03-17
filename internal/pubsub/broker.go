// Package pubsub provides a lightweight in-process broker for fan-out
// event delivery between services and the UI.
//
// Delivery semantics:
//
//   - [Broker.Publish] is best-effort and lossy under contention. If a
//     subscriber's channel is full, the event is dropped for that
//     subscriber, a warning is logged, and a counter is incremented.
//     This is the right choice for high-frequency intermediate updates
//     (e.g. streaming token deltas) where only the latest state
//     matters.
//
//   - [Broker.PublishMustDeliver] is bounded-blocking. For each
//     subscriber it first tries a non-blocking send, then falls back to
//     a per-subscriber blocking send with a hard timeout. On timeout the
//     event is dropped for that subscriber, an error is logged, and the
//     must-deliver drop counter is incremented. The publisher never
//     blocks indefinitely. This is the right choice for terminal events
//     (finish, tool result, error, cancel) that must not be silently
//     coalesced away.
//
// Drop counters ([Broker.DropCount], [Broker.MustDeliverDropCount]) are
// exposed so callers can surface saturation in telemetry.
package pubsub

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// bufferSize is the per-subscriber channel capacity for any broker
	// created via NewBroker. Publish is non-blocking, so a full buffer
	// drops events (with a warning log); sized to cover a long
	// streaming assistant turn (~one UpdatedEvent per token) even under
	// TUI render stalls.
	bufferSize = 4096

	// defaultMustDeliverTimeout is the per-subscriber upper bound on how
	// long [Broker.PublishMustDeliver] will block waiting for buffer
	// space before giving up on that subscriber.
	defaultMustDeliverTimeout = 50 * time.Millisecond
)

// subOptions holds per-subscriber configuration.
type subOptions struct {
	blocking bool
}

type Broker[T any] struct {
	subs                 map[chan Event[T]]subOptions
	mu                   sync.RWMutex
	done                 chan struct{}
	subCount             int
	channelBufferSize    int
	mustDeliverTimeout   time.Duration
	dropCount            atomic.Uint64
	mustDeliverDropCount atomic.Uint64
}

func NewBroker[T any]() *Broker[T] {
	return NewBrokerWithOptions[T](bufferSize)
}

func NewBrokerWithOptions[T any](channelBufferSize int) *Broker[T] {
	return &Broker[T]{
		subs:               make(map[chan Event[T]]subOptions),
		done:               make(chan struct{}),
		channelBufferSize:  channelBufferSize,
		mustDeliverTimeout: defaultMustDeliverTimeout,
	}
}

// SetMustDeliverTimeout overrides the per-subscriber timeout used by
// [Broker.PublishMustDeliver]. A zero or negative value resets to the
// default. Intended primarily for tests.
func (b *Broker[T]) SetMustDeliverTimeout(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if d <= 0 {
		b.mustDeliverTimeout = defaultMustDeliverTimeout
		return
	}
	b.mustDeliverTimeout = d
}

func (b *Broker[T]) Shutdown() {
	select {
	case <-b.done: // Already closed
		return
	default:
		close(b.done)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	for ch := range b.subs {
		delete(b.subs, ch)
		close(ch)
	}

	b.subCount = 0
}

func (b *Broker[T]) Subscribe(ctx context.Context) <-chan Event[T] {
	return b.subscribe(ctx, false)
}

// SubscribeBlocking creates a subscriber whose channel will never have
// events dropped. When the channel buffer is full, Publish blocks until
// there is room. Use this for consumers that must observe every event
// (e.g. the non-interactive stream-json output path).
func (b *Broker[T]) SubscribeBlocking(ctx context.Context) <-chan Event[T] {
	return b.subscribe(ctx, true)
}

func (b *Broker[T]) subscribe(ctx context.Context, blocking bool) <-chan Event[T] {
	b.mu.Lock()
	defer b.mu.Unlock()

	select {
	case <-b.done:
		ch := make(chan Event[T])
		close(ch)
		return ch
	default:
	}

	sub := make(chan Event[T], b.channelBufferSize)
	b.subs[sub] = subOptions{blocking: blocking}
	b.subCount++

	go func() {
		<-ctx.Done()

		b.mu.Lock()
		defer b.mu.Unlock()

		select {
		case <-b.done:
			return
		default:
		}

		delete(b.subs, sub)
		close(sub)
		b.subCount--
	}()

	return sub
}

func (b *Broker[T]) GetSubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.subCount
}

// DropCount returns the cumulative number of events dropped by
// [Broker.Publish] because a subscriber's channel was full.
func (b *Broker[T]) DropCount() uint64 {
	return b.dropCount.Load()
}

// MustDeliverDropCount returns the cumulative number of events dropped
// by [Broker.PublishMustDeliver] after the per-subscriber timeout
// expired.
func (b *Broker[T]) MustDeliverDropCount() uint64 {
	return b.mustDeliverDropCount.Load()
}

// Publish delivers an event to every active subscriber.
//
// Delivery is non-blocking and lossy: if a subscriber's channel is full
// the event is dropped for that subscriber, a warning is logged, and
// [Broker.DropCount] is incremented. Use [Broker.PublishMustDeliver]
// for events that must not be silently dropped.
func (b *Broker[T]) Publish(t EventType, payload T) {
	select {
	case <-b.done:
		return
	default:
	}

	event := Event[T]{Type: t, Payload: payload}

	// Snapshot subscribers under the read-lock. Non-blocking
	// sends happen inline; blocking subscribers are collected
	// so their (potentially slow) sends occur after the lock
	// is released. This prevents a full blocking channel from
	// holding the lock and stalling other publishers or
	// preventing unsubscribe.
	var blockingSubs []chan Event[T]

	b.mu.RLock()
	for sub, opts := range b.subs {
		if opts.blocking {
			blockingSubs = append(blockingSubs, sub)
		} else {
			select {
			case sub <- event:
			default:
				// Channel is full, subscriber is slow — skip this event.
				// Lossy by design; counted and logged so saturation is
				// observable. This prevents blocking the publisher.
				b.dropCount.Add(1)
				slog.Warn("Pubsub buffer full; dropping event", "type", t)
			}
		}
	}
	b.mu.RUnlock()

	for _, sub := range blockingSubs {
		// The channel may have been closed by a concurrent
		// unsubscribe (context cancellation) after we released
		// the lock. Recover from the resulting panic rather than
		// re-acquiring the lock for every send.
		if !safeSend(sub, event, b.done) {
			return
		}
	}
}

// PublishMustDeliver delivers an event with bounded-blocking semantics.
// For each subscriber it first attempts a non-blocking send, then falls
// back to a blocking send bounded by a per-subscriber timeout (default
// [defaultMustDeliverTimeout]). On timeout the event is dropped for
// that subscriber, [Broker.MustDeliverDropCount] is incremented, and an
// error is logged. The publisher never blocks indefinitely.
//
// Use this for terminal events that must reach subscribers (finish,
// tool result, error, cancel). Callers must still tolerate rare drops
// after timeout — recovery is the subscriber's responsibility (e.g. a
// re-fetch on the next session-visible event).
func (b *Broker[T]) PublishMustDeliver(ctx context.Context, t EventType, payload T) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	select {
	case <-b.done:
		return
	default:
	}

	event := Event[T]{Type: t, Payload: payload}
	timeout := b.mustDeliverTimeout

	for sub := range b.subs {
		// Fast path: non-blocking send.
		select {
		case sub <- event:
			continue
		default:
		}

		// Slow path: bounded blocking send.
		timer := time.NewTimer(timeout)
		select {
		case sub <- event:
			timer.Stop()
		case <-timer.C:
			b.mustDeliverDropCount.Add(1)
			slog.Error("PublishMustDeliver timed out delivering event",
				"type", t, "timeout", timeout)
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

// safeSend attempts a blocking channel send. It returns false if the
// broker's done channel fires (caller should stop publishing). If
// the target channel was closed concurrently, the panic is recovered
// and the subscriber is silently skipped.
func safeSend[T any](ch chan Event[T], event Event[T], done <-chan struct{}) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = true
		}
	}()
	select {
	case ch <- event:
		return true
	case <-done:
		return false
	}
}
