package main

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(stripBOM(b), v)
}

// guard only lets the app's own page talk to the API: the Host must be our
// loopback address (blocks DNS rebinding), and state-changing requests need
// the X-App header, which a cross-site page cannot send without a CORS
// preflight we never answer.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "127.0.0.1" && host != "localhost" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-App") != AppName {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("X-App", AppName)
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func newRouter() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))

	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		lastPing.Store(time.Now().UnixNano())
		writeJSON(w, 200, map[string]any{"ok": true, "instance": instanceID})
	})

	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"app":       AppName,
			"version":   Version,
			"instance":  instanceID,
			"installed": installed(),
			"main":      mainSrv.snapshot(),
			"util":      utilSrv.snapshot(),
			"setup":     setup.snapshot(),
			"appDir":    appDir,
		})
	})

	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"config": getConfig(), "defaults": defaultConfig()})
	})
	mux.HandleFunc("PUT /api/config", func(w http.ResponseWriter, r *http.Request) {
		c := getConfig()
		if err := readJSON(r, &c); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if err := saveConfig(c); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"config": getConfig()})
	})

	mux.HandleFunc("GET /api/chats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, store.search(r.URL.Query().Get("q")))
	})
	mux.HandleFunc("POST /api/chats", func(w http.ResponseWriter, r *http.Request) {
		c, err := store.create()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, c)
	})
	mux.HandleFunc("GET /api/chats/{id}", func(w http.ResponseWriter, r *http.Request) {
		c, err := store.load(r.PathValue("id"))
		if err != nil {
			writeErr(w, 404, "chat not found")
			return
		}
		writeJSON(w, 200, map[string]any{"chat": c, "busy": activeGeneration(c.ID) != nil})
	})
	mux.HandleFunc("PATCH /api/chats/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Title string `json:"title"`
		}
		if err := readJSON(r, &body); err != nil || strings.TrimSpace(body.Title) == "" {
			writeErr(w, 400, "title required")
			return
		}
		id := r.PathValue("id")
		l := store.lock(id)
		l.Lock()
		defer l.Unlock()
		c, err := store.load(id)
		if err != nil {
			writeErr(w, 404, "chat not found")
			return
		}
		c.Title = truncateRunes(strings.TrimSpace(body.Title), 120)
		c.TitleAuto = false
		if err := store.save(c); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, c)
	})
	mux.HandleFunc("DELETE /api/chats/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if g := activeGeneration(id); g != nil {
			g.cancel()
		}
		l := store.lock(id)
		l.Lock()
		defer l.Unlock()
		if err := store.delete(id); err != nil {
			writeErr(w, 404, err.Error())
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	mux.HandleFunc("POST /api/chats/{id}/send", handleSend)
	mux.HandleFunc("POST /api/chats/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		if g := activeGeneration(r.PathValue("id")); g != nil {
			g.cancel()
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/chats/{id}/approve", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID    string `json:"id"`
			Allow bool   `json:"allow"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		g := activeGeneration(r.PathValue("id"))
		if g == nil || !g.resolveApproval(body.ID, body.Allow) {
			writeErr(w, 404, "no pending approval")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	mux.HandleFunc("POST /api/restart", func(w http.ResponseWriter, r *http.Request) {
		if !installed() {
			writeErr(w, 409, "engine or models are not installed")
			return
		}
		for _, g := range allGenerations() {
			g.cancel()
		}
		startModels()
		writeJSON(w, 200, map[string]bool{"ok": true})
	})

	mux.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		writeJSON(w, 200, logs.since(since))
	})

	mux.HandleFunc("GET /api/setup/plan", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, setupPlan())
	})
	mux.HandleFunc("POST /api/setup/start", func(w http.ResponseWriter, r *http.Request) {
		var req setupRequest
		if err := readJSON(r, &req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		runSetup(req)
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/setup/finish", func(w http.ResponseWriter, r *http.Request) {
		snap := setup.snapshot()
		if snap["done"] != true {
			writeErr(w, 409, "setup has not finished")
			return
		}
		if exe, _ := snap["relaunch"].(string); exe != "" {
			// Hand over to the installed copy; this window reconnects to it.
			if err := relaunch(exe); err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			writeJSON(w, 200, map[string]any{"relaunch": true})
			go func() {
				time.Sleep(500 * time.Millisecond)
				if quitRequested != nil {
					quitRequested("handing over to the installed copy")
				}
			}()
			return
		}
		setup.reset()
		if installed() {
			startModels()
		}
		writeJSON(w, 200, map[string]any{"relaunch": false})
	})

	mux.HandleFunc("POST /api/open-folder", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Which string `json:"which"`
		}
		_ = readJSON(r, &body)
		dir := map[string]string{"workspace": workspaceDir, "data": dataDir, "app": appDir}[body.Which]
		if dir == "" {
			writeErr(w, 400, "unknown folder")
			return
		}
		if runtime.GOOS == "windows" {
			_ = exec.Command("explorer.exe", dir).Start()
		}
		writeJSON(w, 200, map[string]string{"path": dir})
	})

	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]bool{"ok": true})
		go func() {
			time.Sleep(200 * time.Millisecond)
			if quitRequested != nil {
				quitRequested("quit from the UI")
			}
		}()
	})

	return guard(mux)
}

// handleSend streams one agent turn as server-sent events.
func handleSend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req SendRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	g := &generation{chatID: id, cancel: cancel, approvals: map[string]chan bool{}}
	gensMu.Lock()
	if gens[id] != nil {
		gensMu.Unlock()
		writeErr(w, 409, "this chat is already generating")
		return
	}
	gens[id] = g
	gensMu.Unlock()
	defer func() {
		gensMu.Lock()
		if gens[id] == g {
			delete(gens, id)
		}
		gensMu.Unlock()
	}()

	chat, err := prepareTurn(id, req)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher.Flush()

	var mu sync.Mutex
	emit := func(e event) {
		b, err := json.Marshal(e)
		if err != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(b)
		_, _ = w.Write([]byte("\n\n"))
		flusher.Flush()
	}
	runTurn(ctx, g, chat, req, emit)
	emit(event{"type": "end"})
}
