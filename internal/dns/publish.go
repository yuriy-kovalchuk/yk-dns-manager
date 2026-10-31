package dns

import (
	"context"
	"sync"
	"time"

	"github.com/yuriy-kovalchuk/yk-dns-manager/internal/metrics"
)

// Publisher decides when a provider whose writes are staged must publish them, and spaces those publishes Interval apart.
type Publisher struct {
	interval time.Duration

	// staged counts writes; published is staged as of the last successful publish, so a publish is owed while published < staged.
	mu          sync.Mutex
	staged      uint64
	published   uint64
	lastPublish time.Time
}

// NewPublisher starts with one publish owed: a new process cannot know whether the previous one died with changes staged.
func NewPublisher(interval time.Duration) *Publisher {
	return &Publisher{interval: interval, staged: 1}
}

// Staged records a write that a later publish must cover.
func (p *Publisher) Staged() {
	p.mu.Lock()
	p.staged++
	p.mu.Unlock()
}

// Publish runs publish if a write is owed, or returns *PublishDeferred when the last publish was less than the interval ago.
func (p *Publisher) Publish(ctx context.Context, publish func(context.Context) error) error {
	p.mu.Lock()
	target := p.staged
	owed := p.published < target
	wait := p.interval - time.Since(p.lastPublish)
	p.mu.Unlock()
	if !owed {
		return nil
	}
	if wait > 0 {
		// The writes stay owed, so the next Publish after the wait covers a whole burst at once.
		metrics.Publishes.WithLabelValues("deferred").Inc()
		return &PublishDeferred{After: wait}
	}
	start := time.Now()
	err := publish(ctx)
	metrics.PublishDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		metrics.Publishes.WithLabelValues("failed").Inc()
		return err
	}
	metrics.Publishes.WithLabelValues("ok").Inc()
	// A write staged while publish ran is above target, so it stays owed.
	p.mu.Lock()
	p.published = max(p.published, target)
	p.lastPublish = time.Now()
	p.mu.Unlock()
	return nil
}
