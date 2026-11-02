//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"path/filepath"
)

// Development fallback on Linux/macOS: open the default browser, follow the
// heartbeat.
func openWindow(url string, exited chan struct{}) error {
	_ = exec.Command("xdg-open", url).Start()
	return errors.New("untracked browser window")
}

func createShortcuts(target string) error { return errors.New("shortcuts are only made on Windows") }

func relaunch(exe string) error {
	cmd := exec.Command(exe, "--relaunched")
	cmd.Dir = filepath.Dir(exe)
	return cmd.Start()
}
