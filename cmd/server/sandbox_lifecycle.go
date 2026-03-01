package main

import (
	"fmt"
	"time"
)

type sandboxState uint8

const (
	sandboxStateRunning sandboxState = iota
	sandboxStateClosing
	sandboxStateClosed
)

func (sb *sandbox) tryStartExec() error {
	sb.lifecycleMu.Lock()
	defer sb.lifecycleMu.Unlock()
	if sb.state != sandboxStateRunning {
		return fmt.Errorf("sandbox is closing")
	}
	sb.inFlightExec++
	return nil
}

func (sb *sandbox) finishExec() {
	sb.lifecycleMu.Lock()
	defer sb.lifecycleMu.Unlock()
	if sb.inFlightExec > 0 {
		sb.inFlightExec--
	}
}

func (sb *sandbox) beginDestroy() bool {
	sb.lifecycleMu.Lock()
	defer sb.lifecycleMu.Unlock()
	if sb.state != sandboxStateRunning {
		return false
	}
	sb.state = sandboxStateClosing
	return true
}

func (sb *sandbox) waitForExecDrain(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		sb.lifecycleMu.Lock()
		inFlight := sb.inFlightExec
		sb.lifecycleMu.Unlock()
		if inFlight == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (sb *sandbox) currentInFlightExec() int {
	sb.lifecycleMu.Lock()
	defer sb.lifecycleMu.Unlock()
	return sb.inFlightExec
}

func (sb *sandbox) finishDestroy() {
	sb.lifecycleMu.Lock()
	defer sb.lifecycleMu.Unlock()
	sb.state = sandboxStateClosed
}
