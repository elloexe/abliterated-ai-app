package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Two llama-server processes:
//   main: the 9B on the GPU, with the 0.8B as speculative draft model.
//   util: a second 0.8B on the CPU for chat titles and summaries.

type ServerState string

const (
	StateStopped  ServerState = "stopped"
	StateStarting ServerState = "starting"
	StateReady    ServerState = "ready"
	StateFailed   ServerState = "failed"
)

// launchMode is one rung of the fallback ladder.
type launchMode struct {
	ID    string
	Label string
	Ctx   int
	Draft bool
	Safe  bool
}

func ladder(c Config) []launchMode {
	var modes []launchMode
	full := c.ContextSize
	if c.UseDraft {
		modes = append(modes, launchMode{"spec", fmt.Sprintf("Speculative · %dk", full/1024), full, true, false})
	}
	modes = append(modes, launchMode{"nodraft", fmt.Sprintf("No draft · %dk", full/1024), full, false, false})
	if full > 16384 {
		modes = append(modes, launchMode{"16k", "No draft · 16k", 16384, false, false})
	}
	modes = append(modes, launchMode{"safe", "Safe mode · 8k", 8192, false, true})
	return modes
}

type llamaServer struct {
	name string
	port int

	mu       sync.Mutex
	state    ServerState
	mode     launchMode
	detail   string
	cmd      *exec.Cmd
	done     chan struct{}
	stopping bool
	ctxSize  int
}

var (
	mainSrv = &llamaServer{name: "main", state: StateStopped}
	utilSrv = &llamaServer{name: "util", state: StateStopped}
)

func llamaExe() string {
	return filepath.Join(binDir, "llama-server.exe")
}

func modelPath(name string) string { return filepath.Join(modelsDir, name) }

func (s *llamaServer) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"state":  s.state,
		"mode":   s.mode.Label,
		"modeId": s.mode.ID,
		"draft":  s.mode.Draft,
		"ctx":    s.ctxSize,
		"detail": s.detail,
		"port":   s.port,
	}
}

func (s *llamaServer) ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == StateReady
}

func (s *llamaServer) baseURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(s.port)
}

func (s *llamaServer) setState(st ServerState, detail string) {
	s.mu.Lock()
	s.state = st
	s.detail = detail
	s.mu.Unlock()
}

// stop kills the process (if any) and waits for it to exit.
func (s *llamaServer) stop() {
	s.mu.Lock()
	cmd, done := s.cmd, s.done
	s.stopping = true
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		if done != nil {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
		}
	}
	s.mu.Lock()
	s.cmd = nil
	s.state = StateStopped
	s.stopping = false
	s.mu.Unlock()
}

func mainArgs(c Config, m launchMode) []string {
	args := []string{
		"-m", modelPath(MainModelFile),
		"--host", "127.0.0.1", "--port", strconv.Itoa(c.MainPort),
		"-ngl", "99", "--jinja", "-np", "1",
		"-c", strconv.Itoa(m.Ctx),
		"--alias", "main",
		"--no-webui",
	}
	if m.Safe {
		// Safe mode: no KV-cache quantisation, let llama.cpp pick flash attention.
		args = append(args, "-fa", "auto")
	} else {
		args = append(args, "-fa", "on", "-ctk", "q8_0", "-ctv", "q8_0")
	}
	if m.Draft {
		args = append(args,
			"-md", modelPath(HelperModelFile),
			// Without --spec-type draft-simple, llama.cpp reads the 0.8B as an
			// MTP head and aborts with "GGML_ASSERT ... MTP input row width".
			"--spec-type", "draft-simple",
			"-ngld", "99",
			"-ctkd", "q8_0", "-ctvd", "q8_0",
			"--spec-draft-n-max", strconv.Itoa(c.DraftMax),
			"--spec-draft-n-min", strconv.Itoa(c.DraftMin),
		)
	}
	args = append(args, c.ExtraMainArgs...)
	return args
}

func utilArgs(c Config) []string {
	return []string{
		"-m", modelPath(HelperModelFile),
		"--host", "127.0.0.1", "--port", strconv.Itoa(c.UtilPort),
		"-ngl", "0", "-t", strconv.Itoa(c.UtilThreads),
		"-c", strconv.Itoa(c.UtilContext), "--jinja", "-np", "1",
		"--alias", "util",
		"--no-webui",
	}
}

var fatalMarkers = []string{
	"GGML_ASSERT", "out of memory", "cudaMalloc failed", "failed to load model",
	"error loading model", "unable to allocate", "CUDA error", "invalid argument:",
	"error: unknown argument", "failed to allocate",
}

// launch starts the process with args and waits until /health says ok.
func (s *llamaServer) launch(args []string, timeout time.Duration) error {
	exe := llamaExe()
	if _, err := os.Stat(exe); err != nil {
		return fmt.Errorf("llama-server.exe not found in %s", binDir)
	}
	logf(s.name, "starting: llama-server %s", strings.Join(args, " "))

	fatal := make(chan string, 1)
	w := &lineWriter{source: s.name, onLine: func(line string) {
		for _, m := range fatalMarkers {
			if strings.Contains(line, m) {
				select {
				case fatal <- strings.TrimSpace(line):
				default:
				}
				return
			}
		}
	}}

	cmd := exec.Command(exe, args...)
	cmd.Dir = binDir
	cmd.Stdout = w
	cmd.Stderr = w
	cmd.Env = append(os.Environ(), "LLAMA_ARG_HOST=127.0.0.1")
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	addToJob(cmd)

	done := make(chan struct{})
	s.mu.Lock()
	s.cmd = cmd
	s.done = done
	s.mu.Unlock()

	go func() {
		err := cmd.Wait()
		close(done)
		s.mu.Lock()
		stopping := s.stopping
		current := s.cmd == cmd
		wasReady := s.state == StateReady
		s.mu.Unlock()
		if !stopping && current {
			logf(s.name, "llama-server exited: %v", err)
			if wasReady {
				s.setState(StateFailed, "llama-server stopped unexpectedly; use Restart models")
			}
		}
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return errors.New("llama-server exited during startup")
		case line := <-fatal:
			_ = cmd.Process.Kill()
			<-done
			return errors.New(line)
		case <-time.After(500 * time.Millisecond):
		}
		resp, err := client.Get(s.baseURL() + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
	}
	_ = cmd.Process.Kill()
	<-done
	return fmt.Errorf("llama-server did not become ready within %s", timeout)
}

// startMain walks the fallback ladder until one mode comes up.
func startMain() {
	c := getConfig()
	mainSrv.mu.Lock()
	mainSrv.port = c.MainPort
	mainSrv.mu.Unlock()
	modes := ladder(c)
	for i, m := range modes {
		mainSrv.mu.Lock()
		mainSrv.mode = m
		mainSrv.ctxSize = m.Ctx
		mainSrv.state = StateStarting
		mainSrv.detail = "Loading " + m.Label
		mainSrv.mu.Unlock()
		err := mainSrv.launch(mainArgs(c, m), time.Duration(c.StartupTimeout)*time.Second)
		if err == nil {
			logf("main", "ready in mode: %s", m.Label)
			detail := ""
			if i > 0 {
				detail = "Fell back to " + m.Label
			}
			mainSrv.setState(StateReady, detail)
			return
		}
		logf("main", "mode %q failed: %v", m.Label, err)
		mainSrv.mu.Lock()
		mainSrv.cmd = nil
		mainSrv.mu.Unlock()
	}
	mainSrv.setState(StateFailed, "Every launch mode failed. Check the log in Settings.")
}

func startUtil() {
	c := getConfig()
	utilSrv.mu.Lock()
	utilSrv.port = c.UtilPort
	utilSrv.ctxSize = c.UtilContext
	utilSrv.mode = launchMode{ID: "cpu", Label: fmt.Sprintf("CPU · %d threads", c.UtilThreads), Ctx: c.UtilContext}
	utilSrv.state = StateStarting
	utilSrv.detail = "Loading helper on CPU"
	utilSrv.mu.Unlock()
	if err := utilSrv.launch(utilArgs(c), 120*time.Second); err != nil {
		logf("util", "helper failed to start: %v (titles and summaries fall back to simple rules)", err)
		utilSrv.setState(StateFailed, err.Error())
		return
	}
	logf("util", "helper ready on CPU")
	utilSrv.setState(StateReady, "")
}

var restartMu sync.Mutex

// startModels (re)starts both servers. The main model loads first so the
// helper's file reads don't compete with the 5.6 GB GPU upload.
func startModels() {
	if !restartMu.TryLock() {
		return
	}
	go func() {
		defer restartMu.Unlock()
		mainSrv.stop()
		utilSrv.stop()
		go func() {
			// Start the helper once the main server is up (or after 20s).
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) && mainSrv.snapshot()["state"] == StateStarting {
				time.Sleep(500 * time.Millisecond)
			}
			startUtil()
		}()
		startMain()
		// Keep restartMu until the helper is up too.
		for utilSrv.snapshot()["state"] == StateStarting || utilSrv.snapshot()["state"] == StateStopped {
			time.Sleep(300 * time.Millisecond)
		}
	}()
}

func stopModels() {
	mainSrv.stop()
	utilSrv.stop()
}
