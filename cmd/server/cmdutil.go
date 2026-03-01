package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

func runCmd(name string, args ...string) (string, string, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String(), stderr.String(), fmt.Errorf("%s %v: %w (stderr: %s)", name, args, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), stderr.String(), nil
}
