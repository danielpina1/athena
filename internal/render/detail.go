package render

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/redact"
)

// detailClips are the rune limits a tool call's detail tries in turn, each string clipped to the limit, until the
// whole fits DetailMaxBytes: first every value at DetailValueMaxRunes, at last no long value at all (the keys stay).
var detailClips = []int{DetailValueMaxRunes, 500, 80, 0}

// toolDetail is a tool call's input as compact JSON (StreamItem.detail): every string redacted, the values of
// secret-looking keys masked, each long string clipped, and the whole at most DetailMaxBytes. "_lines" holds the
// full line count of each string it shortened, by its dotted path. It is empty when the input is not an object, or
// when even its keys do not fit.
func toolDetail(raw json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var in map[string]any
	if dec.Decode(&in) != nil || in == nil {
		return ""
	}
	masked := maskTree(in).(map[string]any)
	for _, limit := range detailClips {
		lines := map[string]int{}
		out := clipTree(masked, "", limit, lines).(map[string]any)
		if len(lines) > 0 {
			out["_lines"] = lines
		}
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if enc.Encode(out) != nil {
			return ""
		}
		if s := strings.TrimSuffix(b.String(), "\n"); len(s) <= DetailMaxBytes {
			return s
		}
	}
	return ""
}

// maskTree redacts every string in v and masks the value of every secret-looking key, at any depth. It runs once per
// input, before any clip: a clip never leaves part of a secret.
func maskTree(v any) any {
	switch x := v.(type) {
	case string:
		return redact.String(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			if sensitiveKey(k) {
				out[k] = redacted
			} else {
				out[k] = maskTree(e)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = maskTree(e)
		}
		return out
	}
	return v
}

// clipTree copies v with each string over limit runes cut to limit and an ellipsis, noting the full line count of each
// one it cut in lines under its dotted path.
func clipTree(v any, path string, limit int, lines map[string]int) any {
	join := func(k string) string {
		if path == "" {
			return k
		}
		return path + "." + k
	}
	switch x := v.(type) {
	case string:
		if utf8.RuneCountInString(x) <= limit {
			return x
		}
		lines[path] = lineCount(x)
		return clipRunes(x, limit) + "…"
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = clipTree(e, join(k), limit, lines)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = clipTree(e, join(strconv.Itoa(i)), limit, lines)
		}
		return out
	}
	return v
}

// lineCount is how many lines s has: a final newline ends the last line rather than starting one.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// clipRunes is the first n runes of s.
func clipRunes(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}
