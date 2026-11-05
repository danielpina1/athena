// Command planwatch shows, live in a terminal, how Athena's own development is going: the roadmap's plans and, for
// each plan being built task by task, every task's state, fix rounds and the latest events from its ledger.
//
// It reads only files: docs/superpowers/plans (the roadmap, the plans, the "deferred" files written when a plan
// finishes) and the git-ignored ledgers under .superpowers/sdd/*/progress.md. Run it from the repository root:
//
//	go run ./tools/planwatch            # refresh every 2 s until Ctrl-C
//	go run ./tools/planwatch -once      # print once and exit
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// RoadmapPlan is one row of the roadmap table.
type RoadmapPlan struct {
	Num    int
	Name   string
	Status string // done, active or planned
}

// Task is one task of a plan being built, with what its ledger says about it.
type Task struct {
	Num       int
	Title     string
	State     string // done, active or pending
	Last      string // the ledger's latest line about the task, without the "Task N: " prefix
	FixRounds int
}

// Build is a plan being executed task by task, read from its ledger.
type Build struct {
	PlanFile string
	PlanName string
	Tasks    []Task
	Final    string   // the latest "Final …" ledger line, if any
	Rulings  int      // ledger lines starting with "- Ruling"
	Recent   []string // the latest Task/Final events, oldest first
	Updated  time.Time
}

var (
	roadmapRow = regexp.MustCompile(`^\|\s*(\d+)\s*\|\s*(.+?)\s*\|`)
	taskHead   = regexp.MustCompile(`^### Task (\d+):\s*(.+)$`)
	ledgerTask = regexp.MustCompile(`^Task (\d+):\s*(.+)$`)
	fixRound   = regexp.MustCompile(`fix round \d+/\d+`)
	planNum    = regexp.MustCompile(`-p(\d+)-`)
	boldName   = regexp.MustCompile(`\*\*(.+?)\*\*`)
)

func main() {
	root := flag.String("root", ".", "the athena repository root")
	once := flag.Bool("once", false, "print once and exit")
	interval := flag.Duration("interval", 2*time.Second, "refresh interval")
	flag.Parse()

	if *once {
		render(os.Stdout, *root, width(), false)
		return
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	fmt.Print("\x1b[?25l") // hide the cursor
	defer fmt.Print("\x1b[?25h")
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	for {
		render(os.Stdout, *root, width(), true)
		select {
		case <-stop:
			fmt.Println()
			return
		case <-tick.C:
		}
	}
}

// width is the terminal's column count, 100 when it cannot be read.
func width() int {
	if c, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && c > 20 {
		return c
	}
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	if out, err := cmd.Output(); err == nil {
		if f := strings.Fields(string(out)); len(f) == 2 {
			if c, err := strconv.Atoi(f[1]); err == nil && c > 20 {
				return c
			}
		}
	}
	return 100
}

// ReadRoadmap reads the roadmap table and gives each plan its status: done when the plan's "deferred" file exists
// (written when a plan finishes), active when a ledger names one of its plan files, planned otherwise.
func ReadRoadmap(root string, builds []Build) []RoadmapPlan {
	files, _ := filepath.Glob(filepath.Join(root, "docs/superpowers/plans/*-roadmap.md"))
	if len(files) == 0 {
		return nil
	}
	active := map[int]bool{}
	for _, b := range builds {
		if m := planNum.FindStringSubmatch(b.PlanFile); m != nil {
			n, _ := strconv.Atoi(m[1])
			active[n] = true
		}
	}
	var plans []RoadmapPlan
	for _, line := range lines(files[0]) {
		m := roadmapRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		name := m[2]
		if b := boldName.FindStringSubmatch(name); b != nil {
			name = b[1]
		}
		status := "planned"
		if done, _ := filepath.Glob(filepath.Join(root, fmt.Sprintf("docs/superpowers/plans/*-p%d-deferred.md", n))); len(done) > 0 {
			status = "done"
		} else if active[n] {
			status = "active"
		}
		plans = append(plans, RoadmapPlan{Num: n, Name: name, Status: status})
	}
	return plans
}

// ReadBuilds reads every ledger under .superpowers/sdd.
func ReadBuilds(root string) []Build {
	ledgers, _ := filepath.Glob(filepath.Join(root, ".superpowers/sdd/*/progress.md"))
	var builds []Build
	for _, l := range ledgers {
		if b, ok := readBuild(root, l); ok {
			builds = append(builds, b)
		}
	}
	sort.Slice(builds, func(i, j int) bool { return builds[i].PlanFile < builds[j].PlanFile })
	return builds
}

func readBuild(root, ledger string) (Build, bool) {
	ls := lines(ledger)
	if len(ls) == 0 {
		return Build{}, false
	}
	const prefix = "# SDD ledger — plan: "
	if !strings.HasPrefix(ls[0], prefix) {
		return Build{}, false
	}
	b := Build{PlanFile: strings.TrimSpace(strings.TrimPrefix(ls[0], prefix))}
	if st, err := os.Stat(ledger); err == nil {
		b.Updated = st.ModTime()
	}

	byNum := map[int]*Task{}
	var order []int
	for _, line := range lines(filepath.Join(root, b.PlanFile)) {
		if strings.HasPrefix(line, "# ") && b.PlanName == "" {
			b.PlanName = strings.TrimSuffix(strings.TrimPrefix(line, "# "), " Implementation Plan")
		}
		if m := taskHead.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[1])
			if _, seen := byNum[n]; !seen {
				byNum[n] = &Task{Num: n, Title: m[2], State: "pending"}
				order = append(order, n)
			}
		}
	}
	if b.PlanName == "" {
		b.PlanName = filepath.Base(b.PlanFile)
	}

	for _, line := range ls[1:] {
		switch {
		case strings.HasPrefix(line, "- Ruling"):
			b.Rulings++
		case strings.HasPrefix(line, "Final"):
			b.Final = line
			b.Recent = append(b.Recent, line)
		default:
			m := ledgerTask.FindStringSubmatch(line)
			if m == nil {
				// Work outside the plan's tasks (an owner request, a follow-up) is still news: a plain event line
				// shows in Latest. Bullets, tables, headings and indented lines are detail, not events.
				if line != "" && !strings.ContainsAny(line[:1], "-|# \t") {
					b.Recent = append(b.Recent, line)
				}
				continue
			}
			n, _ := strconv.Atoi(m[1])
			t, ok := byNum[n]
			if !ok {
				t = &Task{Num: n, Title: "(not in plan)", State: "pending"}
				byNum[n] = t
				order = append(order, n)
			}
			t.Last = m[2]
			if fixRound.MatchString(line) {
				t.FixRounds++
			}
			if strings.HasPrefix(m[2], "complete") {
				t.State = "done"
			} else if t.State != "done" {
				t.State = "active"
			}
			b.Recent = append(b.Recent, line)
		}
	}
	sort.Ints(order)
	for _, n := range order {
		b.Tasks = append(b.Tasks, *byNum[n])
	}
	if len(b.Recent) > 6 {
		b.Recent = b.Recent[len(b.Recent)-6:]
	}
	return b, true
}

func lines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

// Styles. Orange marks what is in progress, like the dashboard's accent.
const (
	reset  = "\x1b[0m"
	bold   = "\x1b[1m"
	dim    = "\x1b[2m"
	orange = "\x1b[38;5;202m"
	green  = "\x1b[32m"
)

func render(w io.Writer, root string, cols int, clear bool) {
	builds := ReadBuilds(root)
	roadmap := ReadRoadmap(root, builds)
	var out strings.Builder
	if clear {
		out.WriteString("\x1b[H\x1b[2J")
	}
	line := func(style, s string) {
		out.WriteString(style + clip(s, cols) + reset + "\n")
	}

	branch, commit := gitHead(root)
	line(bold, "Athena · development")
	line(dim, fmt.Sprintf("%s · %s · %s", branch, commit, time.Now().Format("15:04:05")))
	out.WriteString("\n")

	if len(roadmap) > 0 {
		done := 0
		for _, p := range roadmap {
			if p.Status == "done" {
				done++
			}
		}
		line(bold, fmt.Sprintf("Roadmap  %s  %d of %d plans done", bar(done, len(roadmap), 20), done, len(roadmap)))
		for _, p := range roadmap {
			switch p.Status {
			case "done":
				line(green, fmt.Sprintf("  ✓ %2d  %s", p.Num, p.Name))
			case "active":
				line(orange+bold, fmt.Sprintf("  ▶ %2d  %s", p.Num, p.Name))
			default:
				line(dim, fmt.Sprintf("  · %2d  %s", p.Num, p.Name))
			}
		}
		out.WriteString("\n")
	}

	if len(builds) == 0 {
		line(dim, "No plan is being built right now.")
	}
	for _, b := range builds {
		done := 0
		for _, t := range b.Tasks {
			if t.State == "done" {
				done++
			}
		}
		line(bold, b.PlanName)
		line("", fmt.Sprintf("%s  %d of %d tasks · %d rulings · ledger %s", bar(done, len(b.Tasks), 30), done, len(b.Tasks),
			b.Rulings, ago(b.Updated)))
		for _, t := range b.Tasks {
			fix := ""
			if t.FixRounds > 0 {
				fix = fmt.Sprintf("  (%d fix round%s)", t.FixRounds, plural(t.FixRounds))
			}
			switch t.State {
			case "done":
				line(green, fmt.Sprintf("  ✓ %2d  %s%s", t.Num, t.Title, fix))
			case "active":
				line(orange+bold, fmt.Sprintf("  ▶ %2d  %s%s", t.Num, t.Title, fix))
				line(orange, "        "+t.Last)
			default:
				line(dim, fmt.Sprintf("  · %2d  %s", t.Num, t.Title))
			}
		}
		if b.Final != "" {
			line(orange, "  "+b.Final)
		}
		if len(b.Recent) > 0 {
			out.WriteString("\n")
			line(dim, "  Latest")
			for _, r := range b.Recent {
				line(dim, "    "+r)
			}
		}
		out.WriteString("\n")
	}
	if clear {
		line(dim, "Ctrl-C to quit")
	}
	io.WriteString(w, out.String())
}

func gitHead(root string) (string, string) {
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		out, err := cmd.Output()
		if err != nil {
			return "?"
		}
		return strings.TrimSpace(string(out))
	}
	return run("rev-parse", "--abbrev-ref", "HEAD"), run("log", "-1", "--format=%h %s")
}

func bar(done, total, width int) string {
	if total == 0 {
		return "[" + strings.Repeat("·", width) + "]"
	}
	filled := done * width / total
	return "[" + strings.Repeat("█", filled) + strings.Repeat("·", width-filled) + "]"
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// clip cuts s to cols runes so a long ledger line never wraps and breaks the redraw.
func clip(s string, cols int) string {
	r := []rune(s)
	if len(r) <= cols {
		return s
	}
	if cols < 2 {
		return string(r[:cols])
	}
	return string(r[:cols-1]) + "…"
}
