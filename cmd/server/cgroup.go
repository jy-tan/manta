package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func ensureCgroupRoot(root string) error {
	// Simple cgroup v2 check.
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return fmt.Errorf("cgroup v2 not available at /sys/fs/cgroup: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create cgroup root %q: %w", root, err)
	}
	return nil
}

func scavengeCgroups(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		log.Printf("scavenge cgroups: read %q: %v", root, err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cg := filepath.Join(root, e.Name())
		_ = killCgroup(cg)
		if err := removeCgroupDir(cg, 1500*time.Millisecond); err != nil {
			log.Printf("scavenge cgroups: remove %q: %v", cg, err)
		}
	}
}

func killCgroup(cgroupPath string) error {
	if strings.TrimSpace(cgroupPath) == "" {
		return nil
	}
	killFile := filepath.Join(cgroupPath, "cgroup.kill")
	if _, err := os.Stat(killFile); err != nil {
		return fmt.Errorf("cgroup.kill missing for %q: %w", cgroupPath, err)
	}
	if err := os.WriteFile(killFile, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("write cgroup.kill for %q: %w", cgroupPath, err)
	}
	return nil
}

func removeCgroupDir(cgroupPath string, timeout time.Duration) error {
	if strings.TrimSpace(cgroupPath) == "" {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		err := os.Remove(cgroupPath)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		// Typical transient errors while the kernel is still tearing down tasks.
		if errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.ENOTEMPTY) {
			if time.Now().After(deadline) {
				return err
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		return err
	}
}

func movePidToCgroup(cgroupPath string, pid int) error {
	procsFile := filepath.Join(cgroupPath, "cgroup.procs")
	if _, err := os.Stat(procsFile); err != nil {
		return fmt.Errorf("cgroup.procs missing for %q: %w", cgroupPath, err)
	}
	if err := os.WriteFile(procsFile, []byte(fmt.Sprintf("%d\n", pid)), 0o644); err != nil {
		return fmt.Errorf("write cgroup.procs for %q: %w", cgroupPath, err)
	}
	return nil
}
