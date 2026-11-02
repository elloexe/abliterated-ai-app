//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func findBrowser() string {
	if b := getConfig().Browser; b != "" {
		if _, err := os.Stat(b); err == nil {
			return b
		}
	}
	var candidates []string
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LOCALAPPDATA"} {
		base := os.Getenv(env)
		if base == "" {
			continue
		}
		candidates = append(candidates,
			filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"),
		)
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// openWindow shows the UI in its own app window (Edge --app mode with a
// private profile, so the window is its own process and we can tell when it
// closes). exited is closed when that process ends. A non-nil error means the
// window could not be tracked (default-browser fallback).
func openWindow(url string, exited chan struct{}) error {
	browser := findBrowser()
	if browser == "" {
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
		return errors.New("no Edge or Chrome found; opened the default browser")
	}
	profile := filepath.Join(dataDir, "window")
	args := []string{
		"--app=" + url,
		"--user-data-dir=" + profile,
		"--window-size=1280,860",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-features=Translate,msEdgeSidebarV2,msHubApps,EdgeCollections",
		// Keep the heartbeat and streaming alive while minimised.
		"--disable-background-timer-throttling",
		"--disable-renderer-backgrounding",
		"--disable-backgrounding-occluded-windows",
	}
	cmd := exec.Command(browser, args...)
	if err := cmd.Start(); err != nil {
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
		return err
	}
	logf("app", "window: %s", filepath.Base(browser))
	if exited != nil {
		go func() {
			_ = cmd.Wait()
			close(exited)
		}()
	}
	return nil
}

// createShortcuts adds Start menu and desktop shortcuts via WScript.Shell.
func createShortcuts(target string) error {
	ps := `$ErrorActionPreference='Stop'
$sh = New-Object -ComObject WScript.Shell
$t = '` + psQuote(target) + `'
foreach ($dir in @([Environment]::GetFolderPath('Programs'), [Environment]::GetFolderPath('Desktop'))) {
  $lnk = $sh.CreateShortcut((Join-Path $dir '` + AppName + `.lnk'))
  $lnk.TargetPath = $t
  $lnk.WorkingDirectory = Split-Path $t
  $lnk.IconLocation = $t + ',0'
  $lnk.Description = '` + AppName + ` - offline AI chat'
  $lnk.Save()
}`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", ps)
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New(strings.TrimSpace(string(out)) + " " + err.Error())
	}
	return nil
}

func psQuote(s string) string { return strings.ReplaceAll(s, "'", "''") }

// relaunch starts the installed exe; it waits for our port to free up.
func relaunch(exe string) error {
	cmd := exec.Command(exe, "--relaunched")
	cmd.Dir = filepath.Dir(exe)
	return cmd.Start()
}
