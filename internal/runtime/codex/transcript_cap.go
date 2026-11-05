package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/redact"
)

const (
	// maxEnvelopeLine is the longest transcript line the node keeps whole (internal/node maxLineBytes, 256 KiB); a longer
	// one reaches the dashboard as "line too large to show". The envelope adds under 128 bytes to the line.
	maxEnvelopeLine = 256<<10 - 128
	// outputKeep is how much (as JSON) of a command's aggregatedOutput the transcript keeps: the dashboard shows a few
	// KiB of it.
	outputKeep = 64 << 10
	// minCut is the shortest string capLine ever cuts (and so redacts first); shorter ones count as the line's overhead.
	minCut = 1 << 10
	// markReserve is what a cut mark ("\n[cut: <n> bytes]") may take.
	markReserve = 40
	// slack keeps the measured size clear of the cap.
	slack = 64
)

// slot is a string of a decoded line and the way to replace it.
type slot struct {
	key string
	s   string
	set func(string)
}

// capLine keeps a Codex line short enough for the transcript. A line over outputKeep is decoded; each string of at least
// minCut bytes is REDACTED (the transcript then holds no secret that a cut could have split), a commandExecution's
// aggregatedOutput is cut to outputKeep, and when the line is still over maxEnvelopeLine the long strings share what is
// left of it: each gets the same byte budget (the short ones keep what they need) and is cut once. A cut is at a newline
// with a cut mark, and drops a partial last line entirely; the line stays valid JSON. A line that fits is returned as it
// is, and the empty-method stub replaces one whose non-string part alone is too big.
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
	var slots []slot
	collect(v, "", &slots)
	var cuttable []*slot
	for i := range slots {
		sl := &slots[i]
		if len(sl.s) < minCut {
			continue
		}
		sl.s = redact.String(sl.s)
		if sl.key == "aggregatedOutput" {
			sl.s = cutFit(sl.s, outputKeep)
		}
		sl.set(sl.s)
		cuttable = append(cuttable, sl)
	}
	out := encode(v)
	if out != nil && len(out) <= maxEnvelopeLine {
		return out
	}
	if out == nil {
		return stub()
	}
	// Share what the strings may take: the line less everything that is not a long string.
	enc := make([]int, len(cuttable))
	total := 0
	for i, sl := range cuttable {
		enc[i] = jsonLen(sl.s)
		total += enc[i]
	}
	budget := maxEnvelopeLine - slack - (len(out) - total)
	order := make([]int, len(cuttable))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return enc[order[a]] < enc[order[b]] })
	// t is the largest budget per string such that the strings fit: the short ones keep their size, the others get t.
	t, left := 0, budget
	for k, i := range order {
		if share := left / (len(order) - k); enc[i] <= share {
			left -= enc[i]
			continue
		} else {
			t = share
			break
		}
	}
	if t < markReserve+2 {
		return stub()
	}
	for i, sl := range cuttable {
		if enc[i] > t {
			sl.set(cutFit(sl.s, t))
		}
	}
	if out = encode(v); out == nil || len(out) > maxEnvelopeLine {
		return stub()
	}
	return out
}

func stub() []byte { return []byte(`{"method":"athena/line-too-large"}`) }

func encode(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if e.Encode(v) != nil {
		return nil
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

func collect(v any, key string, out *[]slot) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if s, ok := e.(string); ok {
				*out = append(*out, slot{k, s, func(n string) { x[k] = n }})
			} else {
				collect(e, k, out)
			}
		}
	case []any:
		for i, e := range x {
			if s, ok := e.(string); ok {
				*out = append(*out, slot{key, s, func(n string) { x[i] = n }})
			} else {
				collect(e, key, out)
			}
		}
	}
}

// jsonLen is the size of s as a JSON string with its quotes, as the encoder here writes it (no HTML escaping).
func jsonLen(s string) int {
	n := 2
	for i := 0; i < len(s); {
		n += runeJSONLen(s[i:], &i)
	}
	return n
}

// runeJSONLen is the size of the first rune of s as JSON, and advances *i past it.
func runeJSONLen(s string, i *int) int {
	r, size := utf8.DecodeRuneInString(s)
	*i += size
	switch {
	case r == utf8.RuneError && size == 1, r == ' ', r == ' ':
		return 6
	case r == '"', r == '\\', r == '\n', r == '\r', r == '\t', r == '\b', r == '\f':
		return 2
	case r < 0x20:
		return 6
	}
	return size
}

// cutFit keeps the part of s that fits in budget bytes as JSON with a cut mark: whole lines only, so no line is cut in
// two (the last, partial one goes). A string that fits is returned as it is.
func cutFit(s string, budget int) string {
	if jsonLen(s) <= budget {
		return s
	}
	room, n := budget-2-markReserve, 0
	i := 0
	for i < len(s) {
		j := i
		c := runeJSONLen(s[i:], &j)
		if n+c > room {
			break
		}
		n, i = n+c, j
	}
	kept := s[:i]
	if i < len(s) && s[i] == '\n' {
		// the kept part ends at the end of a line
	} else if k := strings.LastIndexByte(kept, '\n'); k >= 0 {
		kept = kept[:k]
	} else {
		kept = ""
	}
	return kept + fmt.Sprintf("\n[cut: %d bytes]", len(s)-len(kept))
}
