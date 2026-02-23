package main

import (
	"fmt"
	"log"
	"sync"
	"time"
)

type netnsPool struct {
	cfg  config
	size int

	ch chan *netnsConfig

	mu   sync.Mutex
	all  []*netnsConfig
	once sync.Once
}

func newNetnsPool(cfg config, size int) *netnsPool {
	return &netnsPool{
		cfg:  cfg,
		size: size,
		ch:   make(chan *netnsConfig, size),
	}
}

func (p *netnsPool) Init() error {
	var initErr error
	p.once.Do(func() {
		start := time.Now()
		for i := 1; i <= p.size; i++ {
			// Use stable pool names; each entry owns its subnet.
			id := fmt.Sprintf("pool-%03d", i)

			// Best-effort cleanup from a previous crashed run. This makes pool init
			// idempotent across restarts.
			_ = cleanupSandboxNetnsAndRouting(p.cfg, &netnsConfig{
				NetnsName:  netnsNameForSandbox(id),
				VethHost:   fmt.Sprintf("veth%03d", i),
				SubnetCIDR: fmt.Sprintf("172.16.%d.0/30", i),
			})

			nc, err := setupSandboxNetnsAndRouting(id, i)
			if err != nil {
				initErr = fmt.Errorf("init netns pool entry %d: %w", i, err)
				return
			}
			nc.Pooled = true
			nc.Subnet = i

			p.mu.Lock()
			p.all = append(p.all, nc)
			p.mu.Unlock()

			p.ch <- nc
		}
		log.Printf("netns pool ready: size=%d took=%s", p.size, time.Since(start))
	})
	return initErr
}

func (p *netnsPool) Acquire(timeout time.Duration) (*netnsConfig, error) {
	if p == nil {
		return nil, fmt.Errorf("netns pool is nil")
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	select {
	case nc := <-p.ch:
		return nc, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timed out acquiring netns from pool after %s", timeout)
	}
}

func (p *netnsPool) Release(nc *netnsConfig) {
	if p == nil || nc == nil {
		return
	}
	// Caller ensures the VM is gone; the netns stays configured and ready.
	p.ch <- nc
}

func (p *netnsPool) Destroy() {
	if p == nil {
		return
	}
	p.mu.Lock()
	all := append([]*netnsConfig(nil), p.all...)
	p.mu.Unlock()

	for _, nc := range all {
		_ = cleanupSandboxNetnsAndRouting(p.cfg, nc)
	}
}
