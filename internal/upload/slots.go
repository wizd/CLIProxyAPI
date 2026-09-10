package upload

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// SlotGate is a process-wide semaphore for large in-memory video payloads.
type SlotGate struct {
	mu sync.Mutex
	ch chan struct{}
}

var currentGate atomic.Pointer[SlotGate]

// SetSlotGate registers the gate used by both the HTTP guard and file expansion.
func SetSlotGate(g *SlotGate) {
	currentGate.Store(g)
}

// CurrentSlotGate returns the process-wide large-payload gate, or nil.
func CurrentSlotGate() *SlotGate {
	return currentGate.Load()
}

// Resize replaces the underlying channel with a new capacity.
func (g *SlotGate) Resize(n int64) {
	if g == nil {
		return
	}
	if n <= 0 {
		n = config.DefaultVideoMaxLargePayloadConcurrency
	}
	g.mu.Lock()
	g.ch = make(chan struct{}, n)
	g.mu.Unlock()
}

// Acquire occupies one slot until the returned release function is called.
func (g *SlotGate) Acquire(ctx context.Context) (func(), error) {
	if g == nil {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	g.mu.Lock()
	ch := g.ch
	g.mu.Unlock()
	if ch == nil {
		return func() {}, nil
	}
	select {
	case ch <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() { <-ch })
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
