// Command fakeclaude stands in for the `claude` binary in tests. It speaks just
// enough of the stream-json protocol: it records its arguments, reads user
// messages from stdin, and replays a captured fixture on stdout.
//
// Behaviour is driven by environment variables so other packages can reuse it:
//
//	ATHENA_FAKE_FIXTURE            path of a .jsonl file replayed after the first stdin line
//	ATHENA_FAKE_REPEAT             replay the fixture this many times (default 1)
//	ATHENA_FAKE_STDIN_LOG          file every stdin line is appended to
//	ATHENA_FAKE_ARGS_LOG           file receiving {"args":[...],"env":{ATHENA_*}} as JSON
//	ATHENA_FAKE_EXIT_MID_TURN      when set, replay half of the fixture, then exit 3
//	ATHENA_FAKE_IGNORE_STDIN_CLOSE when set, keep running after stdin reaches EOF
package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	if p := os.Getenv("ATHENA_FAKE_ARGS_LOG"); p != "" {
		rec := struct {
			Args []string          `json:"args"`
			Env  map[string]string `json:"env"`
		}{Args: os.Args[1:], Env: map[string]string{}}
		for _, kv := range os.Environ() {
			if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "ATHENA_") {
				rec.Env[k] = v
			}
		}
		b, _ := json.Marshal(rec)
		_ = os.WriteFile(p, b, 0o600)
	}

	var stdinLog *os.File
	if p := os.Getenv("ATHENA_FAKE_STDIN_LOG"); p != "" {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			os.Exit(4)
		}
		stdinLog = f
	}

	out := bufio.NewWriter(os.Stdout)
	replayed := false
	r := bufio.NewReaderSize(os.Stdin, 1<<20)
	for {
		line, err := r.ReadString('\n')
		if strings.TrimSpace(line) != "" {
			if stdinLog != nil {
				_, _ = stdinLog.WriteString(strings.TrimRight(line, "\n") + "\n")
			}
			if !replayed && !strings.Contains(line, `"control_request"`) {
				replayed = true
				replay(out)
			}
		}
		if err != nil {
			if err != io.EOF {
				os.Exit(5)
			}
			break
		}
	}
	_ = out.Flush()
	if os.Getenv("ATHENA_FAKE_IGNORE_STDIN_CLOSE") != "" {
		for {
			time.Sleep(time.Hour)
		}
	}
}

func replay(out *bufio.Writer) {
	path := os.Getenv("ATHENA_FAKE_FIXTURE")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		os.Exit(6)
	}
	lines := strings.SplitAfter(string(data), "\n")
	n := 1
	if v, err := strconv.Atoi(os.Getenv("ATHENA_FAKE_REPEAT")); err == nil && v > 0 {
		n = v
	}
	if os.Getenv("ATHENA_FAKE_EXIT_MID_TURN") != "" {
		half := lines[:len(lines)/2]
		for _, l := range half {
			_, _ = out.WriteString(l)
		}
		_ = out.Flush()
		os.Exit(3)
	}
	for i := 0; i < n; i++ {
		for _, l := range lines {
			_, _ = out.WriteString(l)
		}
	}
	_ = out.Flush()
}
