package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Chats are stored one JSON file per chat in data/chats/.

type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type Stats struct {
	Tokens       int     `json:"tokens"`
	PromptTokens int     `json:"prompt_tokens"`
	TokPerSec    float64 `json:"tok_per_sec"`
	PredictedMs  float64 `json:"predicted_ms"`
	DraftN       int     `json:"draft_n"`
	DraftAccept  int     `json:"draft_accepted"`
	DraftedPct   float64 `json:"drafted_pct"`
	Mode         string  `json:"mode"`
	Steps        int     `json:"steps"`
}

type Meta struct {
	Created     int64   `json:"created,omitempty"`
	ThoughtSecs float64 `json:"thought_secs,omitempty"`
	Stats       *Stats  `json:"stats,omitempty"`
	ToolStatus  string  `json:"tool_status,omitempty"` // ok, error, denied
	Stopped     bool    `json:"stopped,omitempty"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Reasoning  string     `json:"reasoning_content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
	Meta       *Meta      `json:"meta,omitempty"`
}

type Chat struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Created  int64     `json:"created"`
	Updated  int64     `json:"updated"`
	Messages []Message `json:"messages"`
	// Summary of messages[:SummaryUpto], written by the helper model when the
	// chat outgrows the context window.
	Summary     string `json:"summary,omitempty"`
	SummaryUpto int    `json:"summary_upto,omitempty"`
	Version     int    `json:"version"`
	TitleAuto   bool   `json:"title_auto,omitempty"`
}

type ChatInfo struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Created int64  `json:"created"`
	Updated int64  `json:"updated"`
	Preview string `json:"preview,omitempty"`
}

type chatStore struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

var store = &chatStore{locks: map[string]*sync.Mutex{}}

var idRe = regexp.MustCompile(`^[a-z0-9-]{6,64}$`)

func chatsDir() string { return filepath.Join(dataDir, "chats") }

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// lock returns the per-chat mutex. Hold it while reading-modifying-writing.
func (s *chatStore) lock(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.locks[id]
	if !ok {
		m = &sync.Mutex{}
		s.locks[id] = m
	}
	return m
}

func (s *chatStore) path(id string) (string, error) {
	if !idRe.MatchString(id) {
		return "", errors.New("bad chat id")
	}
	return filepath.Join(chatsDir(), id+".json"), nil
}

func (s *chatStore) load(id string) (*Chat, error) {
	p, err := s.path(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var c Chat
	if err := json.Unmarshal(stripBOM(b), &c); err != nil {
		return nil, err
	}
	if c.Messages == nil {
		c.Messages = []Message{}
	}
	return &c, nil
}

func (s *chatStore) save(c *Chat) error {
	p, err := s.path(c.ID)
	if err != nil {
		return err
	}
	c.Updated = time.Now().UnixMilli()
	b, err := json.MarshalIndent(c, "", " ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p, b)
}

func (s *chatStore) create() (*Chat, error) {
	now := time.Now().UnixMilli()
	c := &Chat{ID: newID(), Title: "New chat", Created: now, Updated: now, Messages: []Message{}, TitleAuto: true}
	return c, s.save(c)
}

func (s *chatStore) delete(id string) error {
	p, err := s.path(id)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

func (s *chatStore) list() []ChatInfo {
	out := []ChatInfo{}
	entries, err := os.ReadDir(chatsDir())
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		c, err := s.load(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		info := ChatInfo{ID: c.ID, Title: c.Title, Created: c.Created, Updated: c.Updated}
		for _, m := range c.Messages {
			if m.Role == "user" {
				info.Preview = truncateRunes(m.Content, 120)
				break
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	return out
}

// search matches title and message text, case-insensitive.
func (s *chatStore) search(q string) []ChatInfo {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return s.list()
	}
	out := []ChatInfo{}
	for _, info := range s.list() {
		if strings.Contains(strings.ToLower(info.Title), q) {
			out = append(out, info)
			continue
		}
		c, err := s.load(info.ID)
		if err != nil {
			continue
		}
		for _, m := range c.Messages {
			lc := strings.ToLower(m.Content)
			if i := strings.Index(lc, q); i >= 0 {
				start := max(0, i-40)
				if start >= len(m.Content) {
					start = 0
				}
				info.Preview = "…" + truncateRunes(strings.ToValidUTF8(m.Content[start:], ""), 120)
				out = append(out, info)
				break
			}
		}
	}
	return out
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
