package pubsub

import (
	"context"
	"sync"
)

const bufferSize = 64

// subOptions holds per-subscriber configuration.
type subOptions struct {
	blocking bool
}

type Broker[T any] struct {
	subs      map[chan Event[T]]subOptions
	mu        sync.RWMutex
	done      chan struct{}
	subCount  int
	maxEvents int
}

func NewBroker[T any]() *Broker[T] {
	return NewBrokerWithOptions[T](bufferSize, 1000)
}

func NewBrokerWithOptions[T any](channelBufferSize, maxEvents int) *Broker[T] {
	return &Broker[T]{
		subs:      make(map[chan Event[T]]subOptions),
		done:      make(chan struct{}),
		maxEvents: maxEvents,
	}
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

	sub := make(chan Event[T], bufferSize)
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
