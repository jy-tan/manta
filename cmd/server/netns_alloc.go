package main

import (
	"fmt"
	"log"
	"sync/atomic"
	"time"
)

func (s *server) acquireNetns(id string) (*netnsConfig, error) {
	if s == nil {
		return nil, fmt.Errorf("server is nil")
	}
	if s.netnsPool != nil {
		// Prefer the pool for stable low latency, but never hard-fail create
		// just because the pool is exhausted.
		nc, err := s.netnsPool.Acquire(10 * time.Millisecond)
		if err == nil {
			return nc, nil
		}
		log.Printf("netns pool exhausted; falling back to on-demand netns: %v", err)
	}
	subnet := int(atomic.AddUint32(&s.nextSubnet, 1))
	return setupSandboxNetnsAndRouting(id, subnet)
}

func (s *server) releaseNetns(nc *netnsConfig) {
	if s == nil || nc == nil {
		return
	}
	if s.netnsPool != nil && nc.Pooled {
		s.netnsPool.Release(nc)
		return
	}
	_ = cleanupSandboxNetnsAndRouting(s.cfg, nc)
}
