package rt

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestTranscriptOffsetsMatchFile: Append returns the offset each line starts at in the file, counting what the file
// held before, and onLine gets the same offset with the line without its newline. A line with no trailing newline gets
// one, so the next line still starts on a line of its own.
func TestTranscriptOffsetsMatchFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcripts", "T-0001.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{\"earlier\":1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	type seen struct {
		off  int64
		line string
	}
	var mu sync.Mutex
	var got []seen
	tr, err := OpenTranscript(path, func(off int64, line []byte) {
		mu.Lock()
		got = append(got, seen{off, string(line)})
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	in := []string{"{\"type\":\"system\"}\n", "{\"no\":\"newline\"}", "{\"type\":\"result\"}\n"}
	var offs []int64
	for _, l := range in {
		offs = append(offs, tr.Append([]byte(l)))
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\"earlier\":1}\n{\"type\":\"system\"}\n{\"no\":\"newline\"}\n{\"type\":\"result\"}\n"; string(file) != want {
		t.Fatalf("file = %q, want %q", file, want)
	}
	if len(got) != len(in) {
		t.Fatalf("onLine called %d times, want %d", len(got), len(in))
	}
	for i, l := range in {
		l = strings.TrimSuffix(l, "\n")
		if got[i].off != offs[i] || got[i].line != l {
			t.Fatalf("line %d: onLine(%d, %q), Append = %d; want the same offset and %q", i, got[i].off, got[i].line, offs[i], l)
		}
		end := int(offs[i]) + len(l)
		if string(file[offs[i]:end]) != l || file[end] != '\n' {
			t.Fatalf("line %d: offset %d does not start %q in the file", i, offs[i], l)
		}
	}
	if offs[0] != int64(len("{\"earlier\":1}\n")) {
		t.Fatalf("first offset = %d: it ignored what the file held", offs[0])
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("transcript mode = %o, want 0600", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Dir(path)); st.Mode().Perm() != 0o700 {
		t.Fatalf("transcript dir mode = %o, want 0700", st.Mode().Perm())
	}
}

func TestTranscriptWithoutOnLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "t.jsonl")
	tr, err := OpenTranscript(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if off := tr.Append([]byte("a\n")); off != 0 {
		t.Fatalf("first offset = %d", off)
	}
	if off := tr.Append([]byte("bc\n")); off != 2 {
		t.Fatalf("second offset = %d", off)
	}
	_ = tr.Close()
}
