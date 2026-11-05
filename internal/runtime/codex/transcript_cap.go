package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"unicode/utf8"

	"github.com/danielpina1/athena/internal/redact"
	"github.com/danielpina1/athena/internal/render"
	"github.com/danielpina1/athena/internal/runtime/rt"
)

const (
	// maxEnvelopeLine is the longest line capLine lets through: the node keeps whole a transcript line of at most
	// rt.MaxTranscriptLine (a longer one reaches the dashboard as "line too large to show"), and the envelope adds under
	// 128 bytes to the line.
	maxEnvelopeLine = rt.MaxTranscriptLine - 128
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
		// Escapes go first (a code between a name and its value would hide the secret from the redactor), then the
		// secrets; only then may anything be cut.
		sl.s = redact.String(render.StripANSI(sl.s))
		sl.set(sl.s)
		cuttable = append(cuttable, sl)
	}
	out := encode(v)
	if out == nil {
		return stub()
	}
	// Each long string gets a budget, and is cut once at most: a command's output at most outputKeep, and all of them
	// together what the line has left once the rest of it is counted.
	enc := make([]int, len(cuttable))
	capOf := func(sl *slot) int {
		if sl.key == "aggregatedOutput" {
			return outputKeep
		}
		return math.MaxInt
	}
	total, over := 0, false
	eff := make([]int, len(cuttable))
	for i, sl := range cuttable {
		enc[i] = jsonLen(sl.s)
		eff[i] = min(enc[i], capOf(sl))
		total += enc[i]
		over = over || enc[i] > capOf(sl)
	}
	if len(out) <= maxEnvelopeLine && !over {
		return out
	}
	t := math.MaxInt
	if budget := maxEnvelopeLine - slack - (len(out) - total); len(out)-total+sumInts(eff) > maxEnvelopeLine-slack {
		// t is the largest budget per string such that the strings fit: the short ones keep their size, the others get t.
		order := make([]int, len(eff))
		for i := range order {
			order[i] = i
		}
		sort.Slice(order, func(a, b int) bool { return eff[order[a]] < eff[order[b]] })
		left := budget
		for k, i := range order {
			share := left / (len(order) - k)
			if eff[i] <= share {
				left -= eff[i]
				continue
			}
			t = share
			break
		}
		if t < markReserve+2 {
			return stub()
		}
	}
	for i, sl := range cuttable {
		if b := min(t, capOf(sl)); enc[i] > b {
			sl.set(cutFit(sl.s, b))
		}
	}
	if out = encode(v); out == nil || len(out) > maxEnvelopeLine {
		return stub()
	}
	return out
}

func sumInts(a []int) (n int) {
	for _, x := range a {
		n += x
	}
	return n
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

// cutFit keeps the longest part of s that fits in budget bytes as JSON with a cut mark, cut at a rune boundary. s is
// already redacted, so no cut can leave part of a secret. The mark says how many bytes of the stripped, redacted text are gone.
func cutFit(s string, budget int) string {
	room, n, i := budget-2-markReserve, 0, 0
	for i < len(s) {
		j := i
		c := runeJSONLen(s[i:], &j)
		if n+c > room {
			break
		}
		n, i = n+c, j
	}
	return s[:i] + fmt.Sprintf("\n[cut: %d bytes]", len(s)-i)
}
