package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// AppName is the product name shown in the window, shortcuts and logs.
const AppName = "Kestrel"

// Build-time values, set with -ldflags "-X main.Version=... -X main.ReleaseRepo=... -X main.ReleaseTag=...".
var (
	Version     = "dev"
	ReleaseRepo = "elloexe/abliterated-ai-app"
	ReleaseTag  = ""
)

// Model file names inside models/.
const (
	MainModelFile   = "Huihui-Qwen3.5-9B-abliterated.i1-Q4_K_M.gguf"
	HelperModelFile = "Qwen3.5-0.8B-Q8_0.gguf"
)

// Config is config.json in the app folder. Every field has a default, so a
// missing or partial file is fine.
type Config struct {
	UIPort   int `json:"ui_port"`
	MainPort int `json:"main_port"`
	UtilPort int `json:"util_port"`

	ContextSize    int  `json:"context_size"`
	UseDraft       bool `json:"use_draft"`
	DraftMax       int  `json:"draft_max"`
	DraftMin       int  `json:"draft_min"`
	UtilThreads    int  `json:"util_threads"`
	UtilContext    int  `json:"util_context"`
	StartupTimeout int  `json:"startup_timeout_sec"`

	SystemPrompt string  `json:"system_prompt"`
	Temperature  float64 `json:"temperature"`
	TopP         float64 `json:"top_p"`
	TopK         int     `json:"top_k"`
	MaxTokens    int     `json:"max_tokens"`
	MaxSteps     int     `json:"max_steps"`

	Thinking    bool   `json:"thinking"`
	Tools       bool   `json:"tools"`
	AutoApprove bool   `json:"auto_approve"`
	Theme       string `json:"theme"`

	ShellTimeoutSec int      `json:"shell_timeout_sec"`
	ExtraMainArgs   []string `json:"extra_main_args"`
	Browser         string   `json:"browser"` // optional path to msedge.exe / chrome.exe
}

const defaultSystemPrompt = `You are a helpful, knowledgeable assistant running fully offline on the user's own computer.
Answer clearly and directly. Use Markdown for structure and fenced code blocks with a language tag for code.
When a tool would give a better answer (current information, web pages, files, calculations), use it, then answer from its result.
Files you read or write live in the user's workspace folder.`

func defaultConfig() Config {
	return Config{
		UIPort:          47821,
		MainPort:        47822,
		UtilPort:        47823,
		ContextSize:     32768,
		UseDraft:        true,
		DraftMax:        16,
		DraftMin:        2,
		UtilThreads:     4,
		UtilContext:     8192,
		StartupTimeout:  240,
		SystemPrompt:    defaultSystemPrompt,
		Temperature:     0.6,
		TopP:            0.95,
		TopK:            20,
		MaxTokens:       8192,
		MaxSteps:        10,
		Thinking:        true,
		Tools:           true,
		AutoApprove:     false,
		Theme:           "dark",
		ShellTimeoutSec: 60,
	}
}

var (
	cfgMu sync.RWMutex
	cfg   Config
)

func configPath() string { return filepath.Join(appDir, "config.json") }

// stripBOM removes a UTF-8 byte order mark. Notepad and PowerShell's
// Out-File / Set-Content -Encoding UTF8 both write one, and encoding/json
// rejects it.
func stripBOM(b []byte) []byte {
	return bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
}

func loadConfig() {
	c := defaultConfig()
	if b, err := os.ReadFile(configPath()); err == nil {
		if err := json.Unmarshal(stripBOM(b), &c); err != nil {
			logf("app", "config.json is not valid JSON (%v); using defaults", err)
			c = defaultConfig()
		}
	}
	sanitizeConfig(&c)
	cfgMu.Lock()
	cfg = c
	cfgMu.Unlock()
}

func sanitizeConfig(c *Config) {
	d := defaultConfig()
	if c.UIPort <= 0 {
		c.UIPort = d.UIPort
	}
	if c.MainPort <= 0 {
		c.MainPort = d.MainPort
	}
	if c.UtilPort <= 0 {
		c.UtilPort = d.UtilPort
	}
	if c.ContextSize < 2048 {
		c.ContextSize = d.ContextSize
	}
	if c.UtilContext < 1024 {
		c.UtilContext = d.UtilContext
	}
	if c.UtilThreads <= 0 {
		c.UtilThreads = d.UtilThreads
	}
	if c.DraftMax <= 0 {
		c.DraftMax = d.DraftMax
	}
	if c.DraftMin <= 0 {
		c.DraftMin = d.DraftMin
	}
	if c.StartupTimeout < 30 {
		c.StartupTimeout = d.StartupTimeout
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = d.MaxTokens
	}
	if c.MaxSteps <= 0 || c.MaxSteps > 50 {
		c.MaxSteps = d.MaxSteps
	}
	if c.ShellTimeoutSec <= 0 {
		c.ShellTimeoutSec = d.ShellTimeoutSec
	}
	if c.Theme != "light" && c.Theme != "dark" {
		c.Theme = "dark"
	}
}

func getConfig() Config {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfg
}

func saveConfig(c Config) error {
	sanitizeConfig(&c)
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(configPath(), b); err != nil {
		return err
	}
	cfgMu.Lock()
	cfg = c
	cfgMu.Unlock()
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
