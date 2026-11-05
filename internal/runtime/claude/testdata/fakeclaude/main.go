// Command fakeclaude stands in for the `claude` binary in tests. It speaks just
// enough of the stream-json protocol: it records its arguments, reads user
// messages from stdin, and replays a captured fixture on stdout.
//
// Behaviour is driven by environment variables so other packages can reuse it:
//
//	ATHENA_FAKE_FIXTURE            path of a .jsonl file replayed after the first stdin line
//	ATHENA_FAKE_REPEAT             replay the fixture this many times (default 1)
//	ATHENA_FAKE_STDIN_LOG          file every stdin line is appended to (it may name {ticket}, below)
//	ATHENA_FAKE_ARGS_LOG           file receiving {"args":[...],"env":{ATHENA_*}} as JSON
//	ATHENA_FAKE_ARGS_APPEND        file receiving the same record as one JSON line per invocation (appended)
//	ATHENA_FAKE_ENV_LOG            file receiving the whole environment, one NAME=value per line; {ticket} in the
//	                               path is replaced by ATHENA_TICKET (one file per Ticket, the latest session's)
//	ATHENA_FAKE_STDERR             text written to stderr at startup
//	ATHENA_FAKE_EXIT_MID_TURN      when set, replay half of the fixture, then exit 3
//	ATHENA_FAKE_EXIT_MID_TURN_ONCE path of a marker file: the first invocation that finds it missing creates it
//	                               and behaves as ATHENA_FAKE_EXIT_MID_TURN; later ones replay normally
//	ATHENA_FAKE_LATER_FIXTURE      path of a .jsonl file replayed after every later user line (not the first)
//	ATHENA_FAKE_LATER_EXIT_ONCE    path of a marker file: the first invocation that replays the later fixture and finds
//	                               it missing creates it and exits 3 right after that replay
//	ATHENA_FAKE_LATER_HOLD         path of a marker file ({ticket} is ATHENA_TICKET): while it exists, a later
//	                               fixture's result line waits (a test files a real Report mid-turn, then removes it)
//	ATHENA_FAKE_IGNORE_STDIN_CLOSE when set, keep running after stdin reaches EOF
//	ATHENA_FAKE_NO_READ_STDIN      when set, never read stdin (a hung child)
//
// {ticket} in ATHENA_FAKE_FIXTURE or ATHENA_FAKE_LATER_FIXTURE gives each Ticket its own: it is replaced by
// ATHENA_TICKET when the fixture is replayed (so a file written after the session started counts), and when that file
// does not exist, by "default"; with neither there, nothing is replayed.
//
// With --replay-user-messages among its arguments it echoes each user line as the real CLI does when it takes the
// message into a turn ({"type":"user",...,"isReplay":true}, with the line's uuid), before replaying for it.
//
//	ATHENA_FAKE_ECHO_BLOCKS        when set, the echo's content is a list of text blocks with the text trimmed (a CLI
//	                               that reshapes what it echoes), not the string sent
//	ATHENA_FAKE_NO_ECHO            when set, never echo (a message the CLI never takes into a turn)
//	ATHENA_FAKE_HOLD_RESULT        path of a file: a fixture's result line waits until the file exists
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
	if p := os.Getenv("ATHENA_FAKE_ARGS_LOG"); p != "" {
		_ = os.WriteFile(p, b, 0o600)
	}
	if p := os.Getenv("ATHENA_FAKE_ARGS_APPEND"); p != "" {
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			_, _ = f.Write(append(b, '\n'))
			_ = f.Close()
		}
	}
	if p := os.Getenv("ATHENA_FAKE_ENV_LOG"); p != "" {
		p = strings.ReplaceAll(p, "{ticket}", os.Getenv("ATHENA_TICKET"))
		// Written whole and renamed, so a reader never sees half a file.
		tmp := p + ".tmp"
		if os.WriteFile(tmp, []byte(strings.Join(os.Environ(), "\n")+"\n"), 0o600) == nil {
			_ = os.Rename(tmp, p)
		}
	}
	if v := os.Getenv("ATHENA_FAKE_STDERR"); v != "" {
		_, _ = os.Stderr.WriteString(v)
	}
	exitMidTurn := os.Getenv("ATHENA_FAKE_EXIT_MID_TURN") != ""
	if p := os.Getenv("ATHENA_FAKE_EXIT_MID_TURN_ONCE"); p != "" {
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
			_ = f.Close()
			exitMidTurn = true
		}
	}

	if os.Getenv("ATHENA_FAKE_NO_READ_STDIN") != "" {
		for {
			time.Sleep(time.Hour)
		}
	}

	var stdinLog *os.File
	if p := strings.ReplaceAll(os.Getenv("ATHENA_FAKE_STDIN_LOG"), "{ticket}", os.Getenv("ATHENA_TICKET")); p != "" {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			os.Exit(4)
		}
		stdinLog = f
	}

	replayUser := false
	for _, a := range os.Args[1:] {
		replayUser = replayUser || a == "--replay-user-messages"
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
			if !strings.Contains(line, `"control_request"`) {
				if replayUser && os.Getenv("ATHENA_FAKE_NO_ECHO") == "" {
					echo(out, line)
				}
				if !replayed {
					replayed = true
					replay(out, exitMidTurn)
				} else {
					replayLater(out)
				}
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

// fixture is the path a fixture variable names, its {ticket} resolved; "" for none.
func fixture(name string) string {
	p := os.Getenv(name)
	if !strings.Contains(p, "{ticket}") {
		return p
	}
	for _, t := range []string{os.Getenv("ATHENA_TICKET"), "default"} {
		if t == "" {
			continue
		}
		if q := strings.ReplaceAll(p, "{ticket}", t); fileExists(q) {
			return q
		}
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func replay(out *bufio.Writer, exitMidTurn bool) {
	path := fixture("ATHENA_FAKE_FIXTURE")
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
	if exitMidTurn {
		half := lines[:len(lines)/2]
		for _, l := range half {
			_, _ = out.WriteString(l)
		}
		_ = out.Flush()
		os.Exit(3)
	}
	for i := 0; i < n; i++ {
		for _, l := range lines {
			holdResult(out, l)
			_, _ = out.WriteString(l)
		}
	}
	_ = out.Flush()
}

// holdResult waits, before a result line, until ATHENA_FAKE_HOLD_RESULT names a file that exists.
func holdResult(out *bufio.Writer, line string) {
	p := os.Getenv("ATHENA_FAKE_HOLD_RESULT")
	if p == "" || !strings.Contains(line, `"type":"result"`) {
		return
	}
	_ = out.Flush()
	for {
		if _, err := os.Stat(p); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// replayLater replays ATHENA_FAKE_LATER_FIXTURE, then exits 3 once if ATHENA_FAKE_LATER_EXIT_ONCE says so.
func replayLater(out *bufio.Writer) {
	path := fixture("ATHENA_FAKE_LATER_FIXTURE")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		os.Exit(6)
	}
	hold := strings.ReplaceAll(os.Getenv("ATHENA_FAKE_LATER_HOLD"), "{ticket}", os.Getenv("ATHENA_TICKET"))
	for _, l := range strings.SplitAfter(string(data), "\n") {
		if hold != "" && strings.Contains(l, `"type":"result"`) {
			_ = out.Flush()
			for fileExists(hold) {
				time.Sleep(10 * time.Millisecond)
			}
		}
		_, _ = out.WriteString(l)
	}
	_ = out.Flush()
	if p := os.Getenv("ATHENA_FAKE_LATER_EXIT_ONCE"); p != "" {
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
			_ = f.Close()
			os.Exit(3)
		}
	}
}

// echo writes the replay of a user line.
func echo(out *bufio.Writer, line string) {
	var in struct {
		UUID    string `json:"uuid"`
		Message struct {
			Content any `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal([]byte(line), &in) != nil {
		return
	}
	content := in.Message.Content
	if text, ok := content.(string); ok && os.Getenv("ATHENA_FAKE_ECHO_BLOCKS") != "" {
		content = []map[string]any{{"type": "text", "text": strings.TrimSpace(text)}}
	}
	rec := map[string]any{"type": "user", "isReplay": true, "message": map[string]any{"role": "user", "content": content}}
	if in.UUID != "" {
		rec["uuid"] = in.UUID
	}
	b, _ := json.Marshal(rec)
	_, _ = out.Write(append(b, '\n'))
	_ = out.Flush()
}
