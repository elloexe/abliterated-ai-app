package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The agent loop: stream a reply from the 9B, run any tool calls it makes
// (asking the user first for risky ones), feed results back, repeat up to
// MaxSteps times.

type event = map[string]any

type generation struct {
	chatID    string
	cancel    context.CancelFunc
	mu        sync.Mutex
	approvals map[string]chan bool
}

var (
	gensMu sync.Mutex
	gens   = map[string]*generation{}
)

func activeGeneration(chatID string) *generation {
	gensMu.Lock()
	defer gensMu.Unlock()
	return gens[chatID]
}

func (g *generation) awaitApproval(ctx context.Context, id string) bool {
	ch := make(chan bool, 1)
	g.mu.Lock()
	g.approvals[id] = ch
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.approvals, id)
		g.mu.Unlock()
	}()
	select {
	case ok := <-ch:
		return ok
	case <-ctx.Done():
		return false
	}
}

func (g *generation) resolveApproval(id string, allow bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch, ok := g.approvals[id]
	if ok {
		ch <- allow
	}
	return ok
}

// SendRequest is the body of POST /api/chats/{id}/send.
type SendRequest struct {
	Content    string `json:"content"`
	EditIndex  *int   `json:"edit_index"` // replace the user message at this index
	Regenerate bool   `json:"regenerate"` // redo the reply to the last user message
	Thinking   *bool  `json:"thinking"`
	Tools      *bool  `json:"tools"`
}

// persistMessages writes the generation's messages while keeping fields that
// other code may have changed meanwhile (title, summary).
func persistMessages(c *Chat) {
	l := store.lock(c.ID)
	l.Lock()
	defer l.Unlock()
	disk, err := store.load(c.ID)
	if errors.Is(err, os.ErrNotExist) {
		return // deleted while generating
	}
	if err == nil {
		c.Title = disk.Title
		c.TitleAuto = disk.TitleAuto
		if disk.Version == c.Version && disk.SummaryUpto <= len(c.Messages) {
			c.Summary = disk.Summary
			c.SummaryUpto = disk.SummaryUpto
		}
	}
	if c.SummaryUpto > len(c.Messages) {
		c.Summary, c.SummaryUpto = "", 0
	}
	if err := store.save(c); err != nil {
		logf("app", "saving chat %s: %v", c.ID, err)
	}
}

// prepareTurn applies a new message, an edit or a regenerate to the chat.
func prepareTurn(id string, req SendRequest) (*Chat, error) {
	l := store.lock(id)
	l.Lock()
	defer l.Unlock()
	c, err := store.load(id)
	if err != nil {
		return nil, err
	}
	truncate := func(n int) {
		if n < len(c.Messages) {
			c.Messages = c.Messages[:n]
			c.Version++
			if c.SummaryUpto > n {
				c.Summary, c.SummaryUpto = "", 0
			}
		}
	}
	now := time.Now().UnixMilli()
	switch {
	case req.Regenerate:
		last := lastUserIndex(c.Messages)
		if last < 0 {
			return nil, errors.New("nothing to regenerate")
		}
		truncate(last + 1)
	case req.EditIndex != nil:
		i := *req.EditIndex
		if i < 0 || i >= len(c.Messages) || c.Messages[i].Role != "user" {
			return nil, errors.New("can only edit a user message")
		}
		if strings.TrimSpace(req.Content) == "" {
			return nil, errors.New("message is empty")
		}
		truncate(i)
		c.Messages = append(c.Messages, Message{Role: "user", Content: req.Content, Meta: &Meta{Created: now}})
	default:
		if strings.TrimSpace(req.Content) == "" {
			return nil, errors.New("message is empty")
		}
		c.Messages = append(c.Messages, Message{Role: "user", Content: req.Content, Meta: &Meta{Created: now}})
	}
	if err := store.save(c); err != nil {
		return nil, err
	}
	return c, nil
}

func lastUserIndex(msgs []Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return i
		}
	}
	return -1
}

// estTokens is a deliberately pessimistic token estimate (about 3.2 chars
// per token for mixed English and code), used only for context budgeting.
func estTokens(s string) int { return len(s)*10/32 + 1 }

func msgTokens(m Message, withReasoning bool) int {
	n := estTokens(m.Content) + 8
	if withReasoning {
		n += estTokens(m.Reasoning)
	}
	for _, tc := range m.ToolCalls {
		n += estTokens(tc.Function.Name) + estTokens(tc.Function.Arguments) + 10
	}
	return n
}

func systemContent(c Config, chat *Chat) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(c.SystemPrompt))
	fmt.Fprintf(&b, "\n\nToday is %s.", time.Now().Format("Monday, 2 January 2006"))
	if chat.Summary != "" && chat.SummaryUpto > 0 {
		b.WriteString("\n\nSummary of the earlier part of this conversation (older messages are not shown):\n")
		b.WriteString(chat.Summary)
	}
	return b.String()
}

// buildMessages converts the chat into the request's messages, fitting it into
// the context window. Reasoning is sent back only for assistant messages after
// the last user message (the current tool loop), as Qwen's template expects.
func buildMessages(c Config, chat *Chat, ctxSize, reserve int) ([]map[string]any, int, int) {
	msgs := chat.Messages
	lastUser := lastUserIndex(msgs)
	start := 0
	if chat.SummaryUpto > 0 && chat.SummaryUpto <= len(msgs) && msgs[chat.SummaryUpto-1].Role != "tool" {
		start = chat.SummaryUpto
	}
	sys := systemContent(c, chat)
	total := estTokens(sys) + 16
	for i := start; i < len(msgs); i++ {
		total += msgTokens(msgs[i], i > lastUser)
	}
	budget := ctxSize - reserve
	dropped := 0
	// Drop whole old turns (always starting at a user message) until it fits.
	for total > budget && start < lastUser {
		next := start + 1
		for next < lastUser && msgs[next].Role != "user" {
			next++
		}
		for i := start; i < next; i++ {
			total -= msgTokens(msgs[i], i > lastUser)
			dropped++
		}
		start = next
	}
	if dropped > 0 {
		logf("app", "context: left out %d old messages to fit %d tokens (summary pending)", dropped, ctxSize)
	}
	out := []map[string]any{{"role": "system", "content": sys}}
	for i := start; i < len(msgs); i++ {
		m := msgs[i]
		om := map[string]any{"role": m.Role, "content": m.Content}
		switch m.Role {
		case "assistant":
			if len(m.ToolCalls) > 0 {
				om["tool_calls"] = m.ToolCalls
			}
			if i > lastUser && m.Reasoning != "" {
				om["reasoning_content"] = m.Reasoning
			}
		case "tool":
			om["tool_call_id"] = m.ToolCallID
			if m.Name != "" {
				om["name"] = m.Name
			}
		}
		out = append(out, om)
	}
	return out, total, dropped
}

type timings struct {
	PromptN        int     `json:"prompt_n"`
	PredictedN     int     `json:"predicted_n"`
	PredictedMs    float64 `json:"predicted_ms"`
	PredictedPerS  float64 `json:"predicted_per_second"`
	DraftN         int     `json:"draft_n"`
	DraftNAccepted int     `json:"draft_n_accepted"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   *string `json:"content"`
			Reasoning *string `json:"reasoning_content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Timings *timings `json:"timings"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type stepResult struct {
	msg      Message
	timings  *timings
	finish   string
	thoughtS float64
}

var streamClient = &http.Client{} // no timeout: replies can take minutes

// streamStep runs one completion and forwards deltas to emit.
func streamStep(ctx context.Context, body map[string]any, emit func(event)) (stepResult, error) {
	res := stepResult{msg: Message{Role: "assistant", Meta: &Meta{Created: time.Now().UnixMilli()}}}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", mainSrv.baseURL()+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := streamClient.Do(req)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return res, fmt.Errorf("llama-server HTTP %d: %s", resp.StatusCode, extractError(eb))
	}

	type partial struct {
		id, name string
		args     strings.Builder
	}
	var calls []*partial
	var content, reasoning strings.Builder
	var thinkStart, thinkEnd time.Time

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var ch streamChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			continue
		}
		if ch.Error != nil {
			return res, errors.New(ch.Error.Message)
		}
		if ch.Timings != nil {
			res.timings = ch.Timings
		}
		for _, c := range ch.Choices {
			d := c.Delta
			if d.Reasoning != nil && *d.Reasoning != "" {
				if thinkStart.IsZero() {
					thinkStart = time.Now()
				}
				reasoning.WriteString(*d.Reasoning)
				emit(event{"type": "reasoning", "text": *d.Reasoning})
			}
			if d.Content != nil && *d.Content != "" {
				if !thinkStart.IsZero() && thinkEnd.IsZero() {
					thinkEnd = time.Now()
					emit(event{"type": "thinking_done", "secs": thinkEnd.Sub(thinkStart).Seconds()})
				}
				content.WriteString(*d.Content)
				emit(event{"type": "content", "text": *d.Content})
			}
			for _, tc := range d.ToolCalls {
				if !thinkStart.IsZero() && thinkEnd.IsZero() {
					thinkEnd = time.Now()
					emit(event{"type": "thinking_done", "secs": thinkEnd.Sub(thinkStart).Seconds()})
				}
				for len(calls) <= tc.Index {
					calls = append(calls, &partial{})
				}
				p := calls[tc.Index]
				if tc.ID != "" {
					p.id = tc.ID
				}
				if tc.Function.Name != "" {
					p.name = tc.Function.Name
				}
				p.args.WriteString(tc.Function.Arguments)
			}
			if c.FinishReason != nil {
				res.finish = *c.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		res.msg.Content = content.String()
		res.msg.Reasoning = reasoning.String()
		return res, err
	}
	if !thinkStart.IsZero() {
		if thinkEnd.IsZero() {
			thinkEnd = time.Now()
			emit(event{"type": "thinking_done", "secs": thinkEnd.Sub(thinkStart).Seconds()})
		}
		res.thoughtS = thinkEnd.Sub(thinkStart).Seconds()
	}
	res.msg.Content = content.String()
	res.msg.Reasoning = strings.TrimSpace(reasoning.String())
	for i, p := range calls {
		if p.name == "" {
			continue
		}
		id := p.id
		if id == "" {
			id = fmt.Sprintf("call_%d_%d", time.Now().UnixNano()%1e6, i)
		}
		args := strings.TrimSpace(p.args.String())
		if args == "" {
			args = "{}"
		}
		res.msg.ToolCalls = append(res.msg.ToolCalls, ToolCall{ID: id, Type: "function", Function: ToolFunction{Name: p.name, Arguments: args}})
	}
	return res, nil
}

func extractError(b []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return strings.TrimSpace(string(b))
}

// runTurn is the whole agent loop for one user turn.
func runTurn(ctx context.Context, g *generation, chat *Chat, req SendRequest, emit func(event)) {
	c := getConfig()
	thinking := c.Thinking
	if req.Thinking != nil {
		thinking = *req.Thinking
	}
	useTools := c.Tools
	if req.Tools != nil {
		useTools = *req.Tools
	}
	emit(event{"type": "chat", "chat": chat})

	if !mainSrv.ready() {
		emit(event{"type": "error", "message": "The main model is not ready yet (" + fmt.Sprint(mainSrv.snapshot()["detail"]) + ")."})
		return
	}
	snap := mainSrv.snapshot()
	ctxSize, _ := snap["ctx"].(int)
	mode, _ := snap["mode"].(string)
	if ctxSize == 0 {
		ctxSize = c.ContextSize
	}

	total := Stats{Mode: mode}
	var thought float64
	firstAssistant := -1

	for step := 0; step < c.MaxSteps; step++ {
		lastStep := step == c.MaxSteps-1
		reserve := min(c.MaxTokens, ctxSize/4)
		msgs, promptEst, _ := buildMessages(c, chat, ctxSize, reserve)
		maxTok := max(256, min(c.MaxTokens, ctxSize-promptEst-64))
		body := map[string]any{
			"model":                "main",
			"messages":             msgs,
			"stream":               true,
			"temperature":          c.Temperature,
			"top_p":                c.TopP,
			"top_k":                c.TopK,
			"max_tokens":           maxTok,
			"chat_template_kwargs": map[string]any{"enable_thinking": thinking},
			"stream_options":       map[string]any{"include_usage": true},
		}
		if useTools {
			body["tools"] = openAITools()
			if lastStep {
				// Out of steps: the model must answer with what it has.
				body["tool_choice"] = "none"
			}
		}
		emit(event{"type": "assistant_start", "index": len(chat.Messages)})
		res, err := streamStep(ctx, body, emit)
		stopped := ctx.Err() != nil
		if res.timings != nil {
			t := res.timings
			total.Tokens += t.PredictedN
			total.PredictedMs += t.PredictedMs
			total.PromptTokens = t.PromptN
			total.DraftN += t.DraftN
			total.DraftAccept += t.DraftNAccepted
		}
		total.Steps = step + 1
		thought += res.thoughtS
		res.msg.Meta.ThoughtSecs = res.thoughtS
		if err != nil && !stopped {
			if res.msg.Content != "" || res.msg.Reasoning != "" {
				chat.Messages = append(chat.Messages, res.msg)
				persistMessages(chat)
			}
			emit(event{"type": "error", "message": err.Error()})
			logf("app", "generation error: %v", err)
			return
		}
		if stopped {
			res.msg.Meta.Stopped = true
			res.msg.ToolCalls = nil
		}
		if res.msg.Content == "" && res.msg.Reasoning == "" && len(res.msg.ToolCalls) == 0 && !stopped {
			res.msg.Content = "(The model returned an empty reply.)"
		}
		chat.Messages = append(chat.Messages, res.msg)
		if firstAssistant < 0 {
			firstAssistant = len(chat.Messages) - 1
		}
		finalizeStats(&total)
		chat.Messages[len(chat.Messages)-1].Meta.Stats = statsCopy(total)
		persistMessages(chat)
		emit(event{"type": "assistant_end", "message": res.msg, "stats": total, "finish": res.finish})
		if stopped {
			emit(event{"type": "stopped"})
			return
		}
		if len(res.msg.ToolCalls) == 0 || !useTools {
			break
		}

		for _, tc := range res.msg.ToolCalls {
			result, status := runToolCall(ctx, g, c, tc, emit)
			chat.Messages = append(chat.Messages, Message{
				Role: "tool", Content: result, ToolCallID: tc.ID, Name: tc.Function.Name,
				Meta: &Meta{Created: time.Now().UnixMilli(), ToolStatus: status},
			})
			persistMessages(chat)
			emit(event{"type": "tool_result", "id": tc.ID, "name": tc.Function.Name, "result": result, "status": status})
			if ctx.Err() != nil {
				emit(event{"type": "stopped"})
				return
			}
		}
	}
	emit(event{"type": "done", "stats": total, "thought_secs": thought})

	go nameChat(chat.ID)
	go maybeSummarize(chat.ID)
}

func finalizeStats(s *Stats) {
	if s.PredictedMs > 0 {
		s.TokPerSec = float64(s.Tokens) / (s.PredictedMs / 1000)
	}
	if s.Tokens > 0 {
		s.DraftedPct = 100 * float64(s.DraftAccept) / float64(s.Tokens)
		if s.DraftedPct > 100 {
			s.DraftedPct = 100
		}
	}
}

func statsCopy(s Stats) *Stats { return &s }

func runToolCall(ctx context.Context, g *generation, c Config, tc ToolCall, emit func(event)) (string, string) {
	name := tc.Function.Name
	t := findTool(name)
	var args map[string]any
	argErr := json.Unmarshal([]byte(tc.Function.Arguments), &args)
	if args == nil {
		args = map[string]any{}
	}
	needsApproval := t != nil && t.Risky && !c.AutoApprove
	emit(event{"type": "tool_call", "id": tc.ID, "name": name, "arguments": tc.Function.Arguments, "risky": t != nil && t.Risky, "needs_approval": needsApproval && argErr == nil})
	if t == nil {
		return "Error: unknown tool " + name, "error"
	}
	if argErr != nil {
		return "Error: arguments are not valid JSON: " + argErr.Error(), "error"
	}
	if needsApproval {
		logf("tools", "waiting for approval: %s", name)
		if !g.awaitApproval(ctx, tc.ID) {
			logf("tools", "denied: %s", name)
			return "The user denied this tool call. Do not retry it; continue without it or ask the user.", "denied"
		}
		emit(event{"type": "tool_running", "id": tc.ID})
	}
	logf("tools", "%s %s", name, truncateRunes(tc.Function.Arguments, 300))
	started := time.Now()
	out, err := t.Run(ctx, args)
	logf("tools", "%s finished in %s", name, time.Since(started).Round(time.Millisecond))
	if err != nil {
		return "Error: " + err.Error(), "error"
	}
	return out, "ok"
}

// ---------- helper model jobs (CPU) ----------

var utilQueue = make(chan func(), 32)

func init() {
	go func() {
		for job := range utilQueue {
			job()
		}
	}()
}

func enqueueUtil(job func()) {
	select {
	case utilQueue <- job:
	default:
		logf("util", "helper queue full; skipping a side job")
	}
}

var thinkTagRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

var utilClient = &http.Client{Timeout: 180 * time.Second}

func utilChat(ctx context.Context, messages []map[string]any, maxTokens int) (string, error) {
	if !utilSrv.ready() {
		return "", errors.New("helper model not ready")
	}
	body := map[string]any{
		"model":                "util",
		"messages":             messages,
		"max_tokens":           maxTokens,
		"temperature":          0.3,
		"top_p":                0.9,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", utilSrv.baseURL()+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := utilClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("helper HTTP %d: %s", resp.StatusCode, extractError(rb))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rb, &out); err != nil || len(out.Choices) == 0 {
		return "", errors.New("helper returned no choices")
	}
	return strings.TrimSpace(thinkTagRe.ReplaceAllString(out.Choices[0].Message.Content, "")), nil
}

func fallbackTitle(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return truncateRunes(s, 48)
}

func cleanTitle(s string) string {
	s = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	s = strings.TrimPrefix(s, "Title:")
	s = strings.Trim(s, " \"'`*#.")
	return truncateRunes(s, 60)
}

// nameChat gives a new chat a title using the helper on the CPU.
func nameChat(id string) {
	c, err := store.load(id)
	if err != nil || !c.TitleAuto {
		return
	}
	var user, reply string
	for _, m := range c.Messages {
		if m.Role == "user" && user == "" {
			user = m.Content
		}
		if m.Role == "assistant" && reply == "" && m.Content != "" {
			reply = m.Content
		}
	}
	if user == "" {
		return
	}
	apply := func(title string) {
		l := store.lock(id)
		l.Lock()
		defer l.Unlock()
		c, err := store.load(id)
		if err != nil || !c.TitleAuto || title == "" {
			return
		}
		c.Title = title
		c.TitleAuto = false
		_ = store.save(c)
	}
	if !utilSrv.ready() {
		apply(fallbackTitle(user))
		return
	}
	enqueueUtil(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		t, err := utilChat(ctx, []map[string]any{
			{"role": "system", "content": "You name chat conversations. Reply with a short, specific title of 2 to 6 words. No quotes, no punctuation at the end, nothing else."},
			{"role": "user", "content": "Conversation:\nUser: " + truncateRunes(user, 1200) + "\nAssistant: " + truncateRunes(reply, 600) + "\n\nTitle:"},
		}, 24)
		title := cleanTitle(t)
		if err != nil || title == "" {
			if err != nil {
				logf("util", "naming chat failed: %v", err)
			}
			title = fallbackTitle(user)
		}
		apply(title)
		logf("util", "named chat: %s", title)
	})
}

var (
	summarizingMu sync.Mutex
	summarizing   = map[string]bool{}
)

// maybeSummarize folds the oldest turns into a running summary once the chat
// passes 60% of the context window, so the next request still fits without
// the GPU ever waiting for the helper.
func maybeSummarize(id string) {
	c, err := store.load(id)
	if err != nil {
		return
	}
	cf := getConfig()
	ctxSize, _ := mainSrv.snapshot()["ctx"].(int)
	if ctxSize == 0 {
		ctxSize = cf.ContextSize
	}
	budget := ctxSize - min(cf.MaxTokens, ctxSize/4)
	start := c.SummaryUpto
	if start > len(c.Messages) {
		start = 0
	}
	used := estTokens(c.Summary)
	for i := start; i < len(c.Messages); i++ {
		used += msgTokens(c.Messages[i], false)
	}
	if used < budget*6/10 {
		return
	}
	// Cut at a user message roughly halfway through the unsummarised part,
	// always keeping the last two user turns verbatim.
	var userIdx []int
	for i := start; i < len(c.Messages); i++ {
		if c.Messages[i].Role == "user" {
			userIdx = append(userIdx, i)
		}
	}
	if len(userIdx) < 3 {
		return
	}
	half := used / 2
	acc := 0
	cut := userIdx[1]
	for i := start; i < userIdx[len(userIdx)-2]; i++ {
		acc += msgTokens(c.Messages[i], false)
		if acc >= half && i+1 < len(c.Messages) && c.Messages[i+1].Role == "user" {
			cut = i + 1
			break
		}
		if c.Messages[i+1].Role == "user" {
			cut = i + 1
		}
	}
	if cut <= start {
		return
	}

	summarizingMu.Lock()
	if summarizing[id] {
		summarizingMu.Unlock()
		return
	}
	summarizing[id] = true
	summarizingMu.Unlock()

	version := c.Version
	prev := c.Summary
	part := append([]Message(nil), c.Messages[start:cut]...)
	enqueueUtil(func() {
		defer func() {
			summarizingMu.Lock()
			delete(summarizing, id)
			summarizingMu.Unlock()
		}()
		if !utilSrv.ready() {
			return
		}
		// Fit the transcript into the helper's own context.
		limitChars := (cf.UtilContext - 1200) * 3
		per := max(300, limitChars/max(1, len(part)))
		var tb strings.Builder
		for _, m := range part {
			switch m.Role {
			case "user":
				tb.WriteString("User: " + truncateRunes(m.Content, per) + "\n\n")
			case "assistant":
				if m.Content != "" {
					tb.WriteString("Assistant: " + truncateRunes(m.Content, per) + "\n\n")
				}
				for _, tc := range m.ToolCalls {
					tb.WriteString("Assistant used tool " + tc.Function.Name + " " + truncateRunes(tc.Function.Arguments, 200) + "\n\n")
				}
			case "tool":
				tb.WriteString("Tool result (" + m.Name + "): " + truncateRunes(m.Content, min(per, 600)) + "\n\n")
			}
		}
		transcript := truncateRunes(tb.String(), limitChars)
		prompt := "Conversation to summarise:\n\n" + transcript
		if prev != "" {
			prompt = "Existing summary of even earlier messages:\n" + truncateRunes(prev, 3000) + "\n\n" + prompt
		}
		ctx, cancel := context.WithTimeout(context.Background(), 170*time.Second)
		defer cancel()
		started := time.Now()
		sum, err := utilChat(ctx, []map[string]any{
			{"role": "system", "content": "You compress conversations. Write a concise summary (at most 250 words) that keeps facts, names, numbers, decisions, code identifiers, file names and open questions. Merge the existing summary if one is given. Write plain text bullet points, nothing else."},
			{"role": "user", "content": prompt},
		}, 600)
		if err != nil || strings.TrimSpace(sum) == "" {
			logf("util", "summarising failed: %v", err)
			return
		}
		l := store.lock(id)
		l.Lock()
		defer l.Unlock()
		cur, err := store.load(id)
		if err != nil || cur.Version != version || len(cur.Messages) < cut {
			return // the chat was edited meanwhile; the next turn will retry
		}
		cur.Summary = strings.TrimSpace(sum)
		cur.SummaryUpto = cut
		_ = store.save(cur)
		logf("util", "summarised %d old messages in %s", cut-start, time.Since(started).Round(time.Second))
	})
}
