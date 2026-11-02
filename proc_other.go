//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
)

// Non-Windows stubs so the code builds and its tests run on Linux CI.

func hideWindow(cmd *exec.Cmd) {}

func addToJob(cmd *exec.Cmd) {}

func messageBox(title, text string) { fmt.Fprintf(os.Stderr, "%s: %s\n", title, text) }
