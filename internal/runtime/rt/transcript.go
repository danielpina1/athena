package rt

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Transcript is the file a session's raw output lines are appended to, one per line, as the runtime printed them.
// The node opens one per process start and closes it after the session's Wait. Raw lines are never logged: they may
// contain secrets.
type Transcript struct {
	mu     sync.Mutex
	f      *os.File
	onLine func(off int64, line []byte)
	off    int64 // where the next line lands
}

// OpenTranscript opens path for appending, creating its directory 0700 and the file 0600 (an existing one is made so).
// onLine, when set, is called for each appended line after it is written, on the appending goroutine (it must not
// block): off is the byte offset the line starts at in the file, counting what the file held before; line is the line
// without its newline.
func OpenTranscript(path string, onLine func(off int64, line []byte)) (*Transcript, error) {
	if err := PrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open transcript: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, fmt.Errorf("chmod transcript: %w", err)
	}
	t := &Transcript{f: f, onLine: onLine}
	if st, err := f.Stat(); err == nil {
		t.off = st.Size()
	}
	return t, nil
}

// Append writes one line, adding the newline it lacks, and returns the offset it starts at. After a short write the
// file is the truth for where the next line lands. line must not be empty; onLine gets a slice of it.
func (t *Transcript) Append(line []byte) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	at := t.off
	n, _ := t.f.Write(line)
	missing := line[len(line)-1] != '\n'
	if missing {
		m, _ := t.f.Write([]byte{'\n'})
		n += m
	}
	if n != len(line)+btoi(missing) {
		if st, err := t.f.Stat(); err == nil {
			n = int(st.Size() - at)
		}
	}
	t.off = at + int64(n)
	if t.onLine != nil {
		t.onLine(at, bytes.TrimSuffix(line, []byte{'\n'}))
	}
	return at
}

// Close closes the file.
func (t *Transcript) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.f.Close()
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// PrivateDir creates dir 0700, or makes an existing one 0700.
func PrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}
	return nil
}
