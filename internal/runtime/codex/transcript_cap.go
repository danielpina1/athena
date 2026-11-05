package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// maxEnvelopeLine is the longest transcript line the node keeps whole (internal/node maxLineBytes, 256 KiB); a longer
	// one reaches the dashboard as "line too large to show". The envelope adds under 128 bytes to the line.
	maxEnvelopeLine = 256<<10 - 128
	// outputKeep is how much of a command's aggregatedOutput the transcript keeps: the dashboard shows a few KiB of it.
	outputKeep = 64 << 10
)

// capLine keeps a Codex line short enough for the transcript: a commandExecution's aggregatedOutput over outputKeep is
// cut, and if the line is still over maxEnvelopeLine its longest strings are halved until it fits. Each cut is at a
// newline and ends in a cut mark, and the line stays valid JSON. A line that fits is returned as it is.
func capLine(raw []byte) []byte {
	if len(raw) <= outputKeep {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return raw
	}
	cutKey(v, "aggregatedOutput", outputKeep)
	enc := func() []byte {
		var b bytes.Buffer
		e := json.NewEncoder(&b)
		e.SetEscapeHTML(false)
		if e.Encode(v) != nil {
			return nil
		}
		return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	}
	out := enc()
	for i := 0; i < 64 && out != nil && len(out) > maxEnvelopeLine; i++ {
		s, set := longestString(v)
		if set == nil || len(s) < 1024 {
			break
		}
		set(cutAtNewline(s, len(s)/2))
		out = enc()
	}
	if out == nil || len(out) > maxEnvelopeLine {
		return []byte(`{"method":"athena/line-too-large"}`)
	}
	return out
}

func cutKey(v any, key string, keep int) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if s, ok := e.(string); ok && k == key {
				x[k] = cutAtNewline(s, keep)
			} else {
				cutKey(e, key, keep)
			}
		}
	case []any:
		for _, e := range x {
			cutKey(e, key, keep)
		}
	}
}

// longestString is the longest string in v and a function that replaces it; set is nil when v has none.
func longestString(v any) (best string, set func(string)) {
	var walk func(v any)
	consider := func(s string, put func(string)) {
		if set == nil || len(s) > len(best) {
			best, set = s, put
		}
	}
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if s, ok := e.(string); ok {
					consider(s, func(n string) { x[k] = n })
				} else {
					walk(e)
				}
			}
		case []any:
			for i, e := range x {
				if s, ok := e.(string); ok {
					consider(s, func(n string) { x[i] = n })
				} else {
					walk(e)
				}
			}
		}
	}
	walk(v)
	return best, set
}

func cutAtNewline(s string, n int) string {
	if len(s) <= n {
		return s
	}
	end := n
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	t := s[:end]
	if i := strings.LastIndexByte(t, '\n'); i > end/2 {
		t = t[:i]
	}
	return t + fmt.Sprintf("\n[cut: %d bytes]", len(s)-len(t))
}
