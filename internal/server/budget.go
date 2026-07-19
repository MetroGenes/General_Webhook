package server

import (
	"sync"
)

// inFlightBudget limits concurrent webhook handlers and in-flight body bytes.
type inFlightBudget struct {
	mu       sync.Mutex
	maxConns int
	maxBytes int64
	conns    int
	bytes    int64
}

func newInFlightBudget(maxConns, maxBytes int) *inFlightBudget {
	if maxConns <= 0 {
		maxConns = 16
	}
	if maxBytes <= 0 {
		maxBytes = 32 << 20
	}
	return &inFlightBudget{maxConns: maxConns, maxBytes: int64(maxBytes)}
}

func (b *inFlightBudget) acquire(n int64) bool {
	if n < 0 {
		n = 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conns >= b.maxConns {
		return false
	}
	if b.bytes+n > b.maxBytes {
		return false
	}
	b.conns++
	b.bytes += n
	return true
}

func (b *inFlightBudget) release(n int64) {
	if n < 0 {
		n = 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.conns--
	if b.conns < 0 {
		b.conns = 0
	}
	b.bytes -= n
	if b.bytes < 0 {
		b.bytes = 0
	}
}
