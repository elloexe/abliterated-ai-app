//go:build live

package main

import (
	"context"
	"testing"
)

// go test -tags live -run Live -v  (needs internet)
func TestLiveTools(t *testing.T) {
	out, err := toolWebSearch(context.Background(), map[string]any{"query": "llama.cpp speculative decoding"})
	t.Logf("search err=%v\n%s", err, out)
	out, err = toolFetchURL(context.Background(), map[string]any{"url": "https://en.wikipedia.org/wiki/Kestrel"})
	if len(out) > 1500 {
		out = out[:1500]
	}
	t.Logf("fetch err=%v\n%s", err, out)
}
