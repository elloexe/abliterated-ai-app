package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStripBOMConfig(t *testing.T) {
	dir := t.TempDir()
	appDir = dir
	body := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"temperature": 0.9, "top_k": 40}`)...)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	loadConfig()
	c := getConfig()
	if c.Temperature != 0.9 || c.TopK != 40 {
		t.Fatalf("BOM config not read: %+v", c)
	}
	if c.TopP != 0.95 || c.ContextSize != 32768 {
		t.Fatalf("defaults lost: %+v", c)
	}
}

func TestLadder(t *testing.T) {
	c := defaultConfig()
	var ids []string
	for _, m := range ladder(c) {
		ids = append(ids, m.ID)
	}
	if got := strings.Join(ids, ","); got != "spec,nodraft,16k,safe" {
		t.Fatalf("ladder = %s", got)
	}
	args := strings.Join(mainArgs(c, ladder(c)[0]), " ")
	for _, want := range []string{"-ngl 99", "--jinja", "-np 1", "-c 32768", "-fa on", "-ctk q8_0", "-ctv q8_0",
		"--spec-type draft-simple", "-ngld 99", "-ctkd q8_0", "-ctvd q8_0", "--spec-draft-n-max 16", "--spec-draft-n-min 2"} {
		if !strings.Contains(args, want) {
			t.Errorf("speculative args missing %q: %s", want, args)
		}
	}
	if strings.Contains(args, "--draft-max") || strings.Contains(args, "--draft-min") {
		t.Errorf("removed flags used: %s", args)
	}
	if a := strings.Join(mainArgs(c, ladder(c)[1]), " "); strings.Contains(a, "-md ") {
		t.Errorf("no-draft mode has a draft model: %s", a)
	}
	u := strings.Join(utilArgs(c), " ")
	for _, want := range []string{"-ngl 0", "-t 4", "-c 8192", "--jinja"} {
		if !strings.Contains(u, want) {
			t.Errorf("util args missing %q: %s", want, u)
		}
	}
}

func TestResolveWorkspace(t *testing.T) {
	workspaceDir = t.TempDir()
	ok := []string{"a.txt", "sub/b.txt", "./c", ".", ""}
	for _, p := range ok {
		if _, err := resolveWorkspace(p); err != nil {
			t.Errorf("%q rejected: %v", p, err)
		}
	}
	bad := []string{"../x", "sub/../../x", "/etc/passwd", "C:\\Windows\\win.ini", "C:x", "..\\..\\x", "a/../../b"}
	for _, p := range bad {
		if _, err := resolveWorkspace(p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspaceDir, "link")); err == nil {
		if _, err := resolveWorkspace("link/secret.txt"); err == nil {
			t.Errorf("symlink escape accepted")
		}
	}
}

func TestWorkspaceFileTools(t *testing.T) {
	workspaceDir = t.TempDir()
	ctx := context.Background()
	if _, err := toolWriteFile(ctx, map[string]any{"path": "notes/a.md", "content": "hello"}); err != nil {
		t.Fatal(err)
	}
	out, err := toolReadFile(ctx, map[string]any{"path": "notes/a.md"})
	if err != nil || out != "hello" {
		t.Fatalf("read = %q, %v", out, err)
	}
	out, _ = toolListFiles(ctx, map[string]any{})
	if !strings.Contains(out, "notes/") {
		t.Fatalf("list = %q", out)
	}
}

func TestHTMLToText(t *testing.T) {
	page := `<html><head><title>Test page</title><style>.x{}</style></head><body>
	<nav><ul><li><a href="/">Home</a></li><li><a href="/a">About</a></li></ul></nav>
	<ul class="menu"><li><a href="#"><img src="x.png"></a></li><li> </li><li>•</li></ul>
	<main><h1>Heading</h1><p>First   paragraph with <b>bold</b>.</p>
	<ul><li>one</li><li>two</li></ul><script>alert(1)</script>
	<p>` + strings.Repeat("Long text. ", 60) + `</p></main><footer>Copyright</footer></body></html>`
	got := htmlToText([]byte(page))
	for _, want := range []string{"Title: Test page", "# Heading", "First paragraph with bold.", "- one\n- two"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"alert", "Home", "Copyright", ".x{}", "\n-\n", "\n•\n"} {
		if strings.Contains(got, bad) {
			t.Errorf("unexpected %q in:\n%s", bad, got)
		}
	}
}

func TestCleanTextDropsEmptyBullets(t *testing.T) {
	got := cleanText("- \n-\n• \n* \nreal line\n\n\n\n- item")
	if got != "real line\n\n- item" {
		t.Fatalf("got %q", got)
	}
}

func TestParseDDG(t *testing.T) {
	page := `<div class="result results_links"><h2><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fexample.com%2Fpage&rut=x">Example <b>Title</b></a></h2>
	<a class="result__snippet">A snippet.</a></div>
	<div class="result result--ad"><a class="result__a" href="https://ads.example">Ad</a></div>`
	res, err := parseDDG(strings.NewReader(page))
	if err != nil || len(res) != 1 {
		t.Fatalf("res=%v err=%v", res, err)
	}
	if res[0].URL != "https://example.com/page" || res[0].Title != "Example Title" || res[0].Snippet != "A snippet." {
		t.Fatalf("%+v", res[0])
	}
}

func TestRunJS(t *testing.T) {
	out, _ := toolRunJS(context.Background(), map[string]any{"code": "console.log('a', 1+1); ({x: [1,2]})"})
	if !strings.Contains(out, "a 2") || !strings.Contains(out, `Result: {"x":[1,2]}`) {
		t.Fatalf("got %q", out)
	}
	out, _ = toolRunJS(context.Background(), map[string]any{"code": "throw new Error('boom')"})
	if !strings.Contains(out, "boom") {
		t.Fatalf("got %q", out)
	}
}

func TestBuildMessagesReasoningAndTrim(t *testing.T) {
	c := defaultConfig()
	chat := &Chat{Messages: []Message{
		{Role: "user", Content: "first " + strings.Repeat("x", 4000)},
		{Role: "assistant", Content: "answer one", Reasoning: "old thoughts"},
		{Role: "user", Content: "second"},
		{Role: "assistant", Reasoning: "new thoughts", ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: ToolFunction{Name: "get_datetime", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "c1", Name: "get_datetime", Content: "noon"},
	}}
	msgs, _, dropped := buildMessages(c, chat, 32768, 4096)
	if dropped != 0 || len(msgs) != 6 {
		t.Fatalf("dropped=%d len=%d", dropped, len(msgs))
	}
	if _, ok := msgs[2]["reasoning_content"]; ok {
		t.Errorf("reasoning sent for an assistant message before the last user turn")
	}
	if msgs[4]["reasoning_content"] != "new thoughts" {
		t.Errorf("reasoning missing in the current tool loop: %v", msgs[4])
	}
	// A small window forces old turns out, always cutting at a user message.
	msgs, _, dropped = buildMessages(c, chat, 1400, 400)
	if dropped != 2 || msgs[1]["role"] != "user" || msgs[1]["content"] != "second" {
		b, _ := json.Marshal(msgs)
		t.Fatalf("dropped=%d msgs=%s", dropped, b)
	}
}

func TestSummaryStartsAtSummaryPoint(t *testing.T) {
	c := defaultConfig()
	chat := &Chat{Summary: "- earlier stuff", SummaryUpto: 2, Messages: []Message{
		{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"},
		{Role: "user", Content: "c"},
	}}
	msgs, _, _ := buildMessages(c, chat, 32768, 4096)
	if len(msgs) != 2 || !strings.Contains(msgs[0]["content"].(string), "- earlier stuff") {
		t.Fatalf("%v", msgs)
	}
}
