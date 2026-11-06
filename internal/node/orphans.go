package node

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A runtime process runs in its own process group, so a server that dies without stopping its sessions (a kill, a
// crash) leaves them running. The node records each process in <stateDir>/run/<ticket id>.json and the next node
// stops what is still there (reapOrphans) before it starts its own sessions.

const (
	runDir = "run"
	// orphanGrace is how long a recorded group gets to end after SIGTERM before it is killed.
	orphanGrace = 5 * time.Second
)

// processRecord is the content of a run file: a process is the one recorded only while its start time is unchanged,
// because a pid is reused.
type processRecord struct {
	PID     int    `json:"pid"`
	PGID    int    `json:"pgid"`
	Started string `json:"started"`
	Runtime string `json:"runtime"`
}

func runFile(stateDir, ticketID string) string {
	return filepath.Join(stateDir, runDir, ticketID+".json")
}

// recordProcess writes the run file of a Ticket's process, replacing an older one.
func recordProcess(stateDir, ticketID string, pid int, runtime string) error {
	started, alive := processStart(pid)
	if !alive {
		return errors.New("process already gone")
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		pgid = pid
	}
	data, err := json.Marshal(processRecord{PID: pid, PGID: pgid, Started: started, Runtime: runtime})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(stateDir, runDir), 0o700); err != nil {
		return err
	}
	return os.WriteFile(runFile(stateDir, ticketID), data, 0o600)
}

// forgetProcess removes a Ticket's run file once its process is reaped. A file that names another pid belongs to a
// newer process of the Ticket and stays.
func forgetProcess(stateDir, ticketID string, pid int) {
	path := runFile(stateDir, ticketID)
	if data, err := os.ReadFile(path); err == nil {
		var rec processRecord
		if json.Unmarshal(data, &rec) == nil && rec.PID != pid {
			return
		}
	}
	_ = os.Remove(path)
}

// processStart returns the start time of pid as ps prints it, and whether the process runs (a zombie does not).
func processStart(pid int) (started string, alive bool) {
	out, err := exec.Command("ps", "-o", "stat=", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", false
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return "", false
	}
	return strings.Join(fields[1:], " "), !strings.HasPrefix(fields[0], "Z")
}

// reapOrphans stops the process groups the run files record, unless the pid now belongs to another process, and
// removes the files. It sends SIGTERM to every group at once, waits up to orphanGrace for them to end, then kills
// what is left.
//
// A group is signalled only through its live leader: the recorded process, with its start time unchanged, still in
// the recorded group (leadsGroup). When the leader is dead but children of it survive, the group is left alone: with
// no leader to vouch for it the pgid may already name another group (pgids are pids, and pids are reused), and a
// kill(-pgid) would hit whatever runs there. Such children outlive the server until they end on their own; they lost
// their session's stdin and stdout with the leader, and the next session of the Ticket starts a group of its own.
func reapOrphans(stateDir string, log *slog.Logger) {
	if stateDir == "" {
		return
	}
	files, _ := filepath.Glob(filepath.Join(stateDir, runDir, "*.json"))
	var groups []processRecord
	for _, f := range files {
		data, err := os.ReadFile(f)
		var rec processRecord
		if err == nil && json.Unmarshal(data, &rec) == nil && rec.PID > 1 {
			if started, alive := processStart(rec.PID); alive && started == rec.Started {
				if rec.PGID <= 1 {
					rec.PGID = rec.PID
				}
				if !leadsGroup(rec) {
					log.Warn("not stopping a session process left by an earlier server: it no longer leads its group",
						"pid", rec.PID, "runtime", rec.Runtime)
				} else {
					log.Warn("stopping a session process left by an earlier server", "pid", rec.PID, "runtime", rec.Runtime)
					_ = syscall.Kill(-rec.PGID, syscall.SIGTERM)
					groups = append(groups, rec)
				}
			}
		}
		_ = os.Remove(f)
	}
	deadline := time.Now().Add(orphanGrace)
	for _, rec := range groups {
		for time.Now().Before(deadline) {
			if started, alive := processStart(rec.PID); !alive || started != rec.Started {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if started, alive := processStart(rec.PID); alive && started == rec.Started && leadsGroup(rec) {
			log.Warn("killing a session process that ignored SIGTERM", "pid", rec.PID)
			_ = syscall.Kill(-rec.PGID, syscall.SIGKILL)
		}
	}
}

// leadsGroup reports whether the recorded process is still in the recorded process group, so that kill(-PGID) reaches
// the session's group and no other.
func leadsGroup(rec processRecord) bool {
	pgid, err := syscall.Getpgid(rec.PID)
	return err == nil && pgid == rec.PGID
}
