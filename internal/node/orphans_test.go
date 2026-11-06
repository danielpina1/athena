package node

import (
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// groupLeader starts a `sleep 60` in its own process group, reaped in the background like a session's process.
func groupLeader(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	})
	return cmd
}

func TestReapOrphansKillsRecordedGroup(t *testing.T) {
	dir := t.TempDir()
	cmd := groupLeader(t)
	pid := cmd.Process.Pid
	if err := recordProcess(dir, "T-0001", pid, "claude"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, runDir, "T-0001.json")
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("run file: %v, %v; want mode 0600", info, err)
	}
	var rec processRecord
	data, _ := os.ReadFile(file)
	if err := json.Unmarshal(data, &rec); err != nil || rec.PID != pid || rec.PGID != pid || rec.Started == "" || rec.Runtime != "claude" {
		t.Fatalf("record = %+v, %v", rec, err)
	}
	reapOrphans(dir, slog.New(slog.DiscardHandler))
	if _, alive := processStart(pid); alive {
		t.Fatal("the recorded process still runs")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("run file stays: %v", err)
	}
}

func TestReapIgnoresReusedPid(t *testing.T) {
	dir := t.TempDir()
	cmd := groupLeader(t)
	pid := cmd.Process.Pid
	// A record of an earlier process with this pid: another start time.
	data, _ := json.Marshal(processRecord{PID: pid, PGID: pid, Started: "Mon Jan  1 00:00:00 2001", Runtime: "claude"})
	if err := os.MkdirAll(filepath.Join(dir, runDir), 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, runDir, "T-0001.json")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	reapOrphans(dir, slog.New(slog.DiscardHandler))
	time.Sleep(100 * time.Millisecond)
	if _, alive := processStart(pid); !alive {
		t.Fatal("a process that is not the recorded one was killed")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("stale run file stays: %v", err)
	}
}

// A record whose pid is alive with its start time unchanged, but in another process group than recorded, is not
// signalled: kill(-PGID) would reach a group that is not the session's.
func TestReapSkipsProcessOutsideRecordedGroup(t *testing.T) {
	dir := t.TempDir()
	leader, other := groupLeader(t), groupLeader(t)
	if err := recordProcess(dir, "T-0001", leader.Process.Pid, "claude"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, runDir, "T-0001.json")
	var rec processRecord
	data, _ := os.ReadFile(file)
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	rec.PGID = other.Process.Pid // the recorded group is now some other group
	data, _ = json.Marshal(rec)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	reapOrphans(dir, slog.New(slog.DiscardHandler))
	time.Sleep(100 * time.Millisecond)
	if _, alive := processStart(leader.Process.Pid); !alive {
		t.Fatal("a process outside its recorded group was killed")
	}
	if _, alive := processStart(other.Process.Pid); !alive {
		t.Fatal("the group the record named was killed")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("run file stays: %v", err)
	}
}
