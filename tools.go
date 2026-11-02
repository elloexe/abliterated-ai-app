package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/dop251/goja"
	"golang.org/x/net/html"
)

const (
	fetchCap    = 14000
	readFileCap = 30000
	outputCap   = 14000
	userAgent   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36 Edg/130.0"
)

type toolDef struct {
	Name        string
	Description string
	Params      map[string]any
	Risky       bool
	Run         func(ctx context.Context, args map[string]any) (string, error)
}

func obj(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

var toolDefs = []toolDef{
	{
		Name:        "web_search",
		Description: "Search the web with DuckDuckGo. Returns titles, URLs and snippets. Use fetch_url to read a result.",
		Params: obj(map[string]any{
			"query":       str("The search query"),
			"max_results": map[string]any{"type": "integer", "description": "How many results (1-10, default 6)"},
		}, "query"),
		Run: toolWebSearch,
	},
	{
		Name:        "fetch_url",
		Description: "Download a web page and return its readable text (menus, scripts and styles removed, about 14,000 characters at most).",
		Params:      obj(map[string]any{"url": str("Absolute http(s) URL")}, "url"),
		Run:         toolFetchURL,
	},
	{
		Name:        "run_javascript",
		Description: "Run JavaScript (ES2020, no browser or Node APIs) in a sandbox and return console.log output and the value of the last expression. Good for calculations and data processing. Needs user approval.",
		Params:      obj(map[string]any{"code": str("JavaScript source")}, "code"),
		Risky:       true,
		Run:         toolRunJS,
	},
	{
		Name:        "run_shell",
		Description: "Run a Windows PowerShell command in the workspace folder and return its output. Needs user approval.",
		Params:      obj(map[string]any{"command": str("PowerShell command")}, "command"),
		Risky:       true,
		Run:         toolRunShell,
	},
	{
		Name:        "list_files",
		Description: "List files and folders in the workspace (or a subfolder of it).",
		Params:      obj(map[string]any{"path": str("Folder relative to the workspace, default '.'")}),
		Run:         toolListFiles,
	},
	{
		Name:        "read_file",
		Description: "Read a text file from the workspace.",
		Params:      obj(map[string]any{"path": str("File path relative to the workspace")}, "path"),
		Run:         toolReadFile,
	},
	{
		Name:        "write_file",
		Description: "Create or overwrite a text file in the workspace. Needs user approval.",
		Params: obj(map[string]any{
			"path":    str("File path relative to the workspace"),
			"content": str("Full file content"),
		}, "path", "content"),
		Risky: true,
		Run:   toolWriteFile,
	},
	{
		Name:        "get_datetime",
		Description: "Get the current local date, time, weekday and time zone.",
		Params:      obj(map[string]any{}),
		Run:         toolDatetime,
	},
}

func findTool(name string) *toolDef {
	for i := range toolDefs {
		if toolDefs[i].Name == name {
			return &toolDefs[i]
		}
	}
	return nil
}

// openAITools is the "tools" array for /v1/chat/completions.
func openAITools() []map[string]any {
	out := make([]map[string]any, 0, len(toolDefs))
	for _, t := range toolDefs {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Params,
			},
		})
	}
	return out
}

func argString(args map[string]any, key string) string {
	switch v := args[key].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func capText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + fmt.Sprintf("\n\n[truncated: %d more characters]", len(r)-n)
}

var httpClient = &http.Client{Timeout: 25 * time.Second}

// ---------- web_search ----------

func toolWebSearch(ctx context.Context, args map[string]any) (string, error) {
	q := strings.TrimSpace(argString(args, "query"))
	if q == "" {
		return "", errors.New("query is empty")
	}
	n := 6
	if v, ok := args["max_results"].(float64); ok && v >= 1 {
		n = min(int(v), 10)
	}
	form := url.Values{"q": {q}, "kl": {"wt-wt"}}
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://html.duckduckgo.com/html/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("search failed (offline?): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("DuckDuckGo returned HTTP %d", resp.StatusCode)
	}
	results, err := parseDDG(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if len(results) == 0 {
		return "No results found for: " + q, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Search results for %q:\n\n", q)
	for i, r := range results {
		if i >= n {
			break
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", r.Snippet)
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String()), nil
}

type searchResult struct{ Title, URL, Snippet string }

func hasClass(n *html.Node, class string) bool {
	for _, a := range n.Attr {
		if a.Key == "class" {
			for _, c := range strings.Fields(a.Val) {
				if c == class {
					return true
				}
			}
		}
	}
	return false
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

// ddgURL unwraps DuckDuckGo's //duckduckgo.com/l/?uddg=<real url> redirects.
func ddgURL(href string) string {
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if real := u.Query().Get("uddg"); real != "" {
		return real
	}
	return href
}

func parseDDG(r io.Reader) ([]searchResult, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, err
	}
	var out []searchResult
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "div" && hasClass(n, "result") && !hasClass(n, "result--ad") {
			var res searchResult
			var inner func(*html.Node)
			inner = func(m *html.Node) {
				if m.Type == html.ElementNode {
					if m.Data == "a" && hasClass(m, "result__a") {
						res.Title = nodeText(m)
						res.URL = ddgURL(attr(m, "href"))
					} else if hasClass(m, "result__snippet") {
						res.Snippet = nodeText(m)
					}
				}
				for c := m.FirstChild; c != nil; c = c.NextSibling {
					inner(c)
				}
			}
			inner(n)
			if res.URL != "" && res.Title != "" && !strings.Contains(res.URL, "duckduckgo.com/y.js") {
				out = append(out, res)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out, nil
}

// ---------- fetch_url ----------

func toolFetchURL(ctx context.Context, args map[string]any) (string, error) {
	raw := strings.TrimSpace(argString(args, "url"))
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("url must be an absolute http(s) URL")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,application/json;q=0.9,*/*;q=0.5")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch failed (offline?): %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return "", err
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	var text string
	switch {
	case strings.Contains(ct, "html") || (ct == "" && bytes.Contains(bytes.ToLower(body[:min(len(body), 512)]), []byte("<html"))):
		text = htmlToText(body)
	case strings.HasPrefix(ct, "text/") || strings.Contains(ct, "json") || strings.Contains(ct, "xml"):
		text = strings.ToValidUTF8(string(body), "")
	default:
		return fmt.Sprintf("HTTP %d. Content-Type %q is not text (%d bytes); not shown.", resp.StatusCode, ct, len(body)), nil
	}
	header := fmt.Sprintf("URL: %s\nHTTP %d\n\n", resp.Request.URL.String(), resp.StatusCode)
	return header + capText(text, fetchCap), nil
}

var skipTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "svg": true, "template": true,
	"iframe": true, "canvas": true, "head": true, "form": true, "button": true, "select": true,
	"nav": true, "footer": true, "aside": true, "dialog": true,
}

var skipRoles = map[string]bool{"navigation": true, "banner": true, "contentinfo": true, "search": true, "menu": true, "menubar": true}

var blockTags = map[string]bool{
	"p": true, "div": true, "section": true, "article": true, "main": true, "header": true,
	"footer": true, "nav": true, "aside": true, "br": true, "hr": true, "tr": true, "table": true,
	"ul": true, "ol": true, "li": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true,
	"h6": true, "pre": true, "blockquote": true, "figure": true, "figcaption": true, "dt": true,
	"dd": true, "dl": true, "details": true, "summary": true,
}

var (
	spaceRe   = regexp.MustCompile(`[ \t\x{00a0}\x{200b}]+`)
	bulletRe  = regexp.MustCompile(`^[\-\*•·◦▪●|>»›–—]*$`)
	blank3Re  = regexp.MustCompile(`\n{3,}`)
	htmlTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

// htmlToText turns a page into readable text. Links in navigation menus
// often leave behind lines that hold only a bullet; those are dropped.
func htmlToText(body []byte) string {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return strings.ToValidUTF8(string(body), "")
	}
	var b strings.Builder
	if m := htmlTitle.FindSubmatch(body); m != nil {
		t := strings.Join(strings.Fields(html.UnescapeString(string(m[1]))), " ")
		if t != "" {
			b.WriteString("Title: " + t + "\n\n")
		}
	}
	root := mainContent(doc)
	var walk func(n *html.Node, pre bool)
	walk = func(n *html.Node, pre bool) {
		switch n.Type {
		case html.TextNode:
			if pre {
				b.WriteString(n.Data)
			} else {
				b.WriteString(strings.ReplaceAll(n.Data, "\n", " "))
			}
			return
		case html.ElementNode:
			if skipTags[n.Data] {
				return
			}
			if hasAttr(n, "hidden") || strings.EqualFold(attr(n, "aria-hidden"), "true") || skipRoles[attr(n, "role")] {
				return
			}
			if n.Data == "pre" {
				pre = true
			}
			if blockTags[n.Data] {
				b.WriteString("\n")
			}
			switch n.Data {
			case "li":
				b.WriteString("- ")
			case "h1":
				b.WriteString("# ")
			case "h2":
				b.WriteString("## ")
			case "h3", "h4":
				b.WriteString("### ")
			case "td", "th":
				b.WriteString(" | ")
			case "img":
				if alt := strings.TrimSpace(attr(n, "alt")); alt != "" {
					b.WriteString("[image: " + alt + "]")
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, pre)
		}
		// List items and cells end at the next block, so lists stay compact.
		if n.Type == html.ElementNode && blockTags[n.Data] && n.Data != "li" && n.Data != "tr" && n.Data != "br" {
			b.WriteString("\n")
		}
	}
	walk(root, false)
	return cleanText(b.String())
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

// mainContent returns the page's <main> or largest <article> when it holds
// most of the text, else the whole document.
func mainContent(doc *html.Node) *html.Node {
	var best *html.Node
	bestLen := 0
	var find func(*html.Node)
	find = func(n *html.Node) {
		if n.Type == html.ElementNode && (n.Data == "main" || n.Data == "article" || attr(n, "role") == "main") {
			if l := len(nodeText(n)); l > bestLen {
				best, bestLen = n, l
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(doc)
	if best != nil && bestLen > 500 {
		return best
	}
	return doc
}

func cleanText(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(spaceRe.ReplaceAllString(l, " "))
		l = strings.TrimPrefix(l, "| ")
		l = strings.TrimSpace(l)
		// Drop empty menu bullets: "-", "- -", "•", "|" and the like.
		if bulletRe.MatchString(strings.ReplaceAll(l, " ", "")) {
			out = append(out, "")
			continue
		}
		out = append(out, l)
	}
	return strings.TrimSpace(blank3Re.ReplaceAllString(strings.Join(out, "\n"), "\n\n"))
}

// ---------- run_javascript ----------

func toolRunJS(ctx context.Context, args map[string]any) (string, error) {
	code := argString(args, "code")
	if strings.TrimSpace(code) == "" {
		return "", errors.New("code is empty")
	}
	vm := goja.New()
	var out strings.Builder
	logFn := func(prefix string) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			parts := make([]string, len(call.Arguments))
			for i, a := range call.Arguments {
				parts[i] = jsString(vm, a)
			}
			out.WriteString(prefix + strings.Join(parts, " ") + "\n")
			return goja.Undefined()
		}
	}
	console := vm.NewObject()
	_ = console.Set("log", logFn(""))
	_ = console.Set("info", logFn(""))
	_ = console.Set("warn", logFn("warn: "))
	_ = console.Set("error", logFn("error: "))
	_ = vm.Set("console", console)

	timer := time.AfterFunc(10*time.Second, func() { vm.Interrupt("timeout: script ran longer than 10 seconds") })
	defer timer.Stop()
	stop := context.AfterFunc(ctx, func() { vm.Interrupt("stopped") })
	defer stop()

	v, err := vm.RunString(code)
	res := strings.TrimRight(out.String(), "\n")
	if err != nil {
		if res != "" {
			res += "\n"
		}
		return capText(res+"Error: "+err.Error(), outputCap), nil
	}
	if v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
		if res != "" {
			res += "\n"
		}
		res += "Result: " + jsString(vm, v)
	}
	if res == "" {
		res = "(no output)"
	}
	return capText(res, outputCap), nil
}

func jsString(vm *goja.Runtime, v goja.Value) string {
	if v == nil || goja.IsUndefined(v) {
		return "undefined"
	}
	if _, ok := v.Export().(string); ok {
		return v.String()
	}
	if o, ok := v.(*goja.Object); ok && o.ClassName() != "Function" && o.ClassName() != "Error" {
		if j, ok := goja.AssertFunction(vm.Get("JSON").ToObject(vm).Get("stringify")); ok {
			if s, err := j(goja.Undefined(), v); err == nil && !goja.IsUndefined(s) {
				return s.String()
			}
		}
	}
	return v.String()
}

// ---------- run_shell ----------

func toolRunShell(ctx context.Context, args map[string]any) (string, error) {
	command := argString(args, "command")
	if strings.TrimSpace(command) == "" {
		return "", errors.New("command is empty")
	}
	c := getConfig()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.ShellTimeoutSec)*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// Force UTF-8 output so non-ASCII text survives.
		script := "[Console]::OutputEncoding=[System.Text.Encoding]::UTF8; $ProgressPreference='SilentlyContinue'; " + command
		cmd = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	cmd.Dir = workspaceDir
	hideWindow(cmd)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	res := strings.ToValidUTF8(buf.String(), "")
	if ctx.Err() == context.DeadlineExceeded {
		res += fmt.Sprintf("\n[timed out after %ds]", c.ShellTimeoutSec)
	} else if err != nil {
		res += "\n[exit: " + err.Error() + "]"
	}
	if strings.TrimSpace(res) == "" {
		res = "(no output, exit code 0)"
	}
	return capText(strings.TrimSpace(res), outputCap), nil
}

// ---------- workspace files ----------

// resolveWorkspace maps a model-supplied path into workspace/, refusing
// absolute paths, drive letters, ".." escapes and links that point outside.
func resolveWorkspace(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		p = "."
	}
	p = strings.ReplaceAll(p, "\\", "/")
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || filepath.VolumeName(p) != "" || strings.Contains(p, ":") {
		return "", errors.New("path must be relative to the workspace folder")
	}
	root, err := filepath.Abs(workspaceDir)
	if err != nil {
		return "", err
	}
	full := filepath.Join(root, filepath.FromSlash(p))
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes the workspace folder")
	}
	// Resolve links on the longest existing prefix.
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	probe := full
	for {
		if _, err := os.Lstat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	if real, err := filepath.EvalSymlinks(probe); err == nil {
		r, err := filepath.Rel(realRoot, real)
		if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return "", errors.New("path leads outside the workspace folder")
		}
	}
	return full, nil
}

func toolListFiles(ctx context.Context, args map[string]any) (string, error) {
	dir, err := resolveWorkspace(argString(args, "path"))
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "(empty folder)", nil
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})
	var b strings.Builder
	for i, e := range entries {
		if i >= 500 {
			fmt.Fprintf(&b, "... and %d more\n", len(entries)-i)
			break
		}
		if e.IsDir() {
			fmt.Fprintf(&b, "%s/\n", e.Name())
			continue
		}
		size := int64(0)
		if info, err := e.Info(); err == nil {
			size = info.Size()
		}
		fmt.Fprintf(&b, "%s  (%s)\n", e.Name(), humanBytes(size))
	}
	return strings.TrimSpace(b.String()), nil
}

func toolReadFile(ctx context.Context, args map[string]any) (string, error) {
	p, err := resolveWorkspace(argString(args, "path"))
	if err != nil {
		return "", err
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", errors.New("that is a folder; use list_files")
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil {
		return "", err
	}
	b = stripBOM(b)
	if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
		return "", fmt.Errorf("%s looks like a binary file (%s)", filepath.Base(p), humanBytes(st.Size()))
	}
	return capText(strings.ToValidUTF8(string(b), ""), readFileCap), nil
}

func toolWriteFile(ctx context.Context, args map[string]any) (string, error) {
	p, err := resolveWorkspace(argString(args, "path"))
	if err != nil {
		return "", err
	}
	content := argString(args, "content")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return "", err
	}
	rel, _ := filepath.Rel(workspaceDir, p)
	return fmt.Sprintf("Wrote %s (%s).", filepath.ToSlash(rel), humanBytes(int64(len(content)))), nil
}

func toolDatetime(ctx context.Context, args map[string]any) (string, error) {
	now := time.Now()
	zone, offset := now.Zone()
	return fmt.Sprintf("Local time: %s\nWeekday: %s\nISO 8601: %s\nTime zone: %s (UTC%+.1f)\nUnix: %d",
		now.Format("2006-01-02 15:04:05"), now.Weekday(), now.Format(time.RFC3339), zone,
		float64(offset)/3600, now.Unix()), nil
}

func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%d B", n)
	case n < k*k:
		return fmt.Sprintf("%.1f KB", float64(n)/k)
	case n < k*k*k:
		return fmt.Sprintf("%.1f MB", float64(n)/(k*k))
	default:
		return fmt.Sprintf("%.2f GB", float64(n)/(k*k*k))
	}
}
