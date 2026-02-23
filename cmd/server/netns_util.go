package main

import (
	"runtime"

	"github.com/vishvananda/netns"
)

func withNetns(target netns.NsHandle, fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	orig, err := netns.Get()
	if err != nil {
		return err
	}
	defer orig.Close()

	if err := netns.Set(target); err != nil {
		return err
	}
	defer netns.Set(orig)

	return fn()
}
