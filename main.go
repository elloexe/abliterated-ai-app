package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	exePath      string
	exeDir       string
	appDir       string // folder holding bin/, models/, data/, workspace/, config.json
	binDir       string
	modelsDir    string
	dataDir      string
	workspaceDir string

	instanceID = strconv.FormatInt(time.Now().UnixNano(), 36)
)

func initPaths(dirFlag string) {
	exePath, _ = os.Executable()
	if p, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = p
	}
	exeDir = filepath.Dir(exePath)
	appDir = exeDir
	if env := os.Getenv("KESTREL_HOME"); env != "" {
		appDir = env
	}
	if dirFlag != "" {
		appDir = dirFlag
	}
	appDir, _ = filepath.Abs(appDir)
	binDir = filepath.Join(appDir, "bin")
	modelsDir = filepath.Join(appDir, "models")
	dataDir = filepath.Join(appDir, "data")
	workspaceDir = filepath.Join(appDir, "workspace")
}

func main() {
	dirFlag := flag.String("dir", "", "app folder (default: the exe's folder)")
	noWindow := flag.Bool("no-window", false, "do not open a window (serve the UI only)")
	relaunched := flag.Bool("relaunched", false, "internal: started by setup after installing")
	flag.Parse()

	initPaths(*dirFlag)
	if !installed() && !samePath(appDir, defaultInstallDir()) {
		// Not set up yet and probably running from Downloads: keep the setup
		// window's profile and logs out of that folder.
		dataDir = filepath.Join(os.TempDir(), AppName+"-setup")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		// The exe sits somewhere read-only (e.g. inside a zip preview).
		messageBox(AppName, "Cannot write to "+appDir+".\n\nExtract or move "+AppName+".exe to a normal folder first.\n\n"+err.Error())
		os.Exit(1)
	}
	if installed() {
		_ = os.MkdirAll(filepath.Join(dataDir, "chats"), 0o755)
		_ = os.MkdirAll(workspaceDir, 0o755)
	}
	initLogFile()
	loadConfig()
	c := getConfig()
	logf("app", "%s %s starting in %s", AppName, Version, appDir)

	uiURL := fmt.Sprintf("http://127.0.0.1:%d/", c.UIPort)

	// Single instance: if we are already running, just open another window.
	if !*relaunched && pingExisting(c.UIPort) {
		logf("app", "already running; opening a window")
		_ = openWindow(uiURL, nil)
		return
	}

	ln, err := listenRetry(c.UIPort, *relaunched)
	if err != nil {
		messageBox(AppName, fmt.Sprintf("Port %d is in use by another program.\n\nChange ui_port in %s.\n\n%v", c.UIPort, configPath(), err))
		os.Exit(1)
	}

	srv := &http.Server{Handler: newRouter(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logf("app", "http server: %v", err)
		}
	}()
	logf("app", "UI at %s", uiURL)

	if installed() {
		startModels()
	} else {
		logf("app", "engine or models missing: showing setup")
	}

	quit := make(chan string, 4)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { quit <- fmt.Sprint("signal ", <-sig) }()
	quitRequested = func(reason string) { quit <- reason }

	switch {
	case *noWindow:
		logf("app", "running without a window (--no-window)")
	case *relaunched:
		// The setup window reconnects to us by itself; follow it by heartbeat.
		go heartbeatWatchdog(quit, 3*time.Minute)
	default:
		exited := make(chan struct{})
		tracked := openWindow(uiURL, exited) == nil
		go windowWatchdog(quit, exited, tracked)
	}

	reason := <-quit
	logf("app", "shutting down: %s", reason)
	shutdown(srv)
}

var quitRequested func(string)

var shutdownOnce sync.Once

func shutdown(srv *http.Server) {
	shutdownOnce.Do(func() {
		for _, g := range allGenerations() {
			g.cancel()
		}
		stopModels()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		logf("app", "bye")
	})
}

func allGenerations() []*generation {
	gensMu.Lock()
	defer gensMu.Unlock()
	out := make([]*generation, 0, len(gens))
	for _, g := range gens {
		out = append(out, g)
	}
	return out
}

func pingExisting(port int) bool {
	client := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200 && resp.Header.Get("X-App") == AppName
}

// listenRetry binds the UI port. After a relaunch the previous process may
// still be closing, so keep trying for a while.
func listenRetry(port int, wait bool) (net.Listener, error) {
	deadline := time.Now().Add(20 * time.Second)
	for {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil || !wait || time.Now().After(deadline) {
			return ln, err
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// lastPing is when the UI last called /api/ping (unix nanos).
var lastPing atomic.Int64

// windowWatchdog quits when the app window closes. When the browser process
// can't be tracked (it handed off to a running browser), fall back to the UI
// heartbeat.
func windowWatchdog(quit chan<- string, exited <-chan struct{}, tracked bool) {
	if tracked {
		<-exited
		// If pings keep coming, the window lives in another browser process.
		time.Sleep(4 * time.Second)
		if time.Since(time.Unix(0, lastPing.Load())) > 12*time.Second {
			quit <- "window closed"
			return
		}
		logf("app", "browser process handed off; following the UI heartbeat instead")
	}
	heartbeatWatchdog(quit, 3*time.Minute)
}

func heartbeatWatchdog(quit chan<- string, firstWait time.Duration) {
	start := time.Now()
	prev := time.Now()
	for {
		time.Sleep(5 * time.Second)
		if time.Since(prev) > 30*time.Second {
			// The computer was asleep; give the window time to reconnect.
			lastPing.Store(time.Now().UnixNano())
		}
		prev = time.Now()
		lp := lastPing.Load()
		if lp == 0 {
			if time.Since(start) > firstWait {
				quit <- "no window connected"
				return
			}
			continue
		}
		// Generous: background windows get their timers throttled.
		if time.Since(time.Unix(0, lp)) > 75*time.Second {
			quit <- "window heartbeat stopped"
			return
		}
	}
}
