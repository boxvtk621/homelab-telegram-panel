package harnesstunnel

import (
	"context"
	"errors"
	"sync"
)

var (
	ErrPoolFull = errors.New("tunnel pool queue is full")
	ErrPoolStop = errors.New("tunnel pool is stopped")
)

// PoolConfig keeps long-lived streams from consuming every connection slot.
// Values are deliberately small because one adapter serves a bounded HomeLab,
// not an untrusted public fan-out.
type PoolConfig struct {
	Total, Streams, PerNode, PerNodeStreams, Pending int
}

func DefaultPoolConfig() PoolConfig {
	return PoolConfig{Total: 16, Streams: 12, PerNode: 4, PerNodeStreams: 3, Pending: 64}
}

func (c PoolConfig) validate() bool {
	return c.Total >= 2 && c.Streams >= 1 && c.Streams < c.Total &&
		c.PerNode >= 2 && c.PerNode <= c.Total && c.PerNodeStreams >= 1 &&
		c.PerNodeStreams < c.PerNode && c.PerNodeStreams <= c.Streams && c.Pending >= 1 && c.Pending <= 4096
}

type poolWaiter struct {
	node, purpose string
	stream        bool
	ready         chan struct{}
	granted       bool
}

type nodeUsage struct{ total, streams int }

type Pool struct {
	mu      sync.Mutex
	config  PoolConfig
	total   int
	streams int
	nodes   map[string]nodeUsage
	waiters []*poolWaiter
	stopped bool
}

func NewPool(config PoolConfig) (*Pool, error) {
	if !config.validate() {
		return nil, errors.New("invalid tunnel pool configuration")
	}
	return &Pool{config: config, nodes: make(map[string]nodeUsage)}, nil
}

type Lease struct {
	pool   *Pool
	node   string
	stream bool
	once   sync.Once
}

func (p *Pool) Acquire(ctx context.Context, node string, purpose Purpose) (*Lease, error) {
	if p == nil || !uuidPattern.MatchString(node) || !purpose.Valid() {
		return nil, errors.New("invalid tunnel pool request")
	}
	w := &poolWaiter{node: node, purpose: string(purpose), stream: purpose.Stream(), ready: make(chan struct{})}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil, ErrPoolStop
	}
	// A full queue can consist entirely of streams that cannot consume the
	// reserved control capacity. Admit a request that is grantable now before
	// applying the pending-waiter bound, otherwise those streams can starve a
	// command, health check, or administrative control.
	if p.canGrantLocked(w) {
		p.grantLocked(w)
		p.mu.Unlock()
		return &Lease{pool: p, node: node, stream: w.stream}, nil
	}
	if len(p.waiters) >= p.config.Pending {
		p.mu.Unlock()
		return nil, ErrPoolFull
	}
	p.waiters = append(p.waiters, w)
	p.scheduleLocked()
	p.mu.Unlock()

	select {
	case <-w.ready:
		if !w.granted {
			return nil, ErrPoolStop
		}
		return &Lease{pool: p, node: node, stream: w.stream}, nil
	case <-ctx.Done():
		p.mu.Lock()
		if !w.granted {
			for index, candidate := range p.waiters {
				if candidate == w {
					p.waiters = append(p.waiters[:index], p.waiters[index+1:]...)
					break
				}
			}
			p.mu.Unlock()
			return nil, ctx.Err()
		}
		p.mu.Unlock()
		// The grant won the race. Return a lease so the caller can release it.
		return &Lease{pool: p, node: node, stream: w.stream}, nil
	}
}

func (p *Pool) canGrantLocked(w *poolWaiter) bool {
	usage := p.nodes[w.node]
	if p.total >= p.config.Total || usage.total >= p.config.PerNode {
		return false
	}
	return !w.stream || p.streams < p.config.Streams && usage.streams < p.config.PerNodeStreams
}

func (p *Pool) scheduleLocked() {
	for index := 0; index < len(p.waiters); {
		w := p.waiters[index]
		if !p.canGrantLocked(w) {
			index++
			continue
		}
		p.waiters = append(p.waiters[:index], p.waiters[index+1:]...)
		p.grantLocked(w)
	}
}

func (p *Pool) grantLocked(w *poolWaiter) {
	usage := p.nodes[w.node]
	usage.total++
	p.total++
	if w.stream {
		usage.streams++
		p.streams++
	}
	p.nodes[w.node] = usage
	w.granted = true
	close(w.ready)
}

func (l *Lease) Release() {
	if l == nil || l.pool == nil {
		return
	}
	l.once.Do(func() {
		p := l.pool
		p.mu.Lock()
		usage := p.nodes[l.node]
		usage.total--
		p.total--
		if l.stream {
			usage.streams--
			p.streams--
		}
		if usage.total == 0 {
			delete(p.nodes, l.node)
		} else {
			p.nodes[l.node] = usage
		}
		p.scheduleLocked()
		p.mu.Unlock()
	})
}

func (p *Pool) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if !p.stopped {
		p.stopped = true
		for _, waiter := range p.waiters {
			close(waiter.ready)
		}
		p.waiters = nil
	}
	p.mu.Unlock()
}
