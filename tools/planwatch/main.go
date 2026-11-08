// Command planwatch shows, live in a terminal, how Athena's own development is going: what is happening now, what
// comes next, what waits on the owner, the plan being built task by task, and the roadmap.
//
// It reads files: the git-ignored .superpowers/now.md (see now.go), docs/superpowers/plans (the roadmap, the plans,
// the "deferred" files written when a plan finishes) and the git-ignored ledgers under .superpowers/sdd/*/progress.md.
// It also asks gh for the open pull requests, every 30 s. Run it from the repository root:
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
	Status string // done, review (built, pull request open), active or planned
	PR     int    // the open pull request of a plan in review
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

// Finished says the plan's tasks are all done and its final review has closed.
func (b Build) Finished() bool {
	for _, t := range b.Tasks {
		if t.State != "done" {
			return false
		}
	}
	return len(b.Tasks) > 0 && strings.Contains(b.Final, "re-review") && !strings.Contains(b.Final, "dispatched")
}

var (
	roadmapRow = regexp.MustCompile(`^\|\s*(\d+)\s*\|\s*(.+?)\s*\|`)
	taskHead   = regexp.MustCompile(`^### Task (\d+):\s*(.+)$`)
	ledgerTask = regexp.MustCompile(`^Task (\d+):\s*(.+)$`)
	fixRound   = regexp.MustCompile(`fix round \d+/\d+`)
	planNum    = regexp.MustCompile(`-p(\d+)-`)
	branchPlan = regexp.MustCompile(`^p(\d+)-`)
	builtNote  = regexp.MustCompile(`\*\*Plan (\d+) is built\*\*`)
	boldName   = regexp.MustCompile(`\*\*(.+?)\*\*`)
)

func main() {
	root := flag.String("root", ".", "the athena repository root")
	once := flag.Bool("once", false, "print once and exit")
	interval := flag.Duration("interval", 2*time.Second, "refresh interval")
	flag.Parse()

	if *once {
		prs, _ := fetchPRs(*root)
		cols, _ := size()
		render(os.Stdout, *root, prs, cols, 0, false)
		return
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	fmt.Print("\x1b[?1049h\x1b[?25l") // a screen of its own, so redraws never pile up in the scrollback; no cursor
	defer fmt.Print("\x1b[?25h\x1b[?1049l")
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	var prs []PR
	var asked time.Time
	for {
		if time.Since(asked) >= prInterval {
			asked = time.Now()
			if fresh, ok := fetchPRs(*root); ok {
				prs = fresh
			}
		}
		cols, rows := size()
		render(os.Stdout, *root, prs, cols, rows, true)
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}

// prInterval is how often GitHub is asked for open pull requests.
const prInterval = 30 * time.Second

// size is the terminal's columns and rows, 100 by 40 when it cannot be read.
func size() (cols, rows int) {
	cols, rows = 100, 40
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	if out, err := cmd.Output(); err == nil {
		if f := strings.Fields(string(out)); len(f) == 2 {
			if r, err := strconv.Atoi(f[0]); err == nil && r > 5 {
				rows = r
			}
			if c, err := strconv.Atoi(f[1]); err == nil && c > 20 {
				cols = c
			}
		}
	}
	if c, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && c > 20 {
		cols = c
	}
	return cols, rows
}

// ReadRoadmap reads the roadmap table and gives each plan its status. A plan is done when its "deferred" file exists
// (written when a plan finishes), the roadmap says "**Plan N is built**", or its ledger is finished; done plans with
// an open pull request on a pN-… branch are in review. A plan with an unfinished ledger is active; the rest are planned.
func ReadRoadmap(root string, builds []Build, prs []PR) []RoadmapPlan {
	files, _ := filepath.Glob(filepath.Join(root, "docs/superpowers/plans/*-roadmap.md"))
	if len(files) == 0 {
		return nil
	}
	active, finished, built, inReview := map[int]bool{}, map[int]bool{}, map[int]bool{}, map[int]int{}
	for _, b := range builds {
		if m := planNum.FindStringSubmatch(b.PlanFile); m != nil {
			n, _ := strconv.Atoi(m[1])
			if b.Finished() {
				finished[n] = true
			} else {
				active[n] = true
			}
		}
	}
	for _, p := range prs {
		if m := branchPlan.FindStringSubmatch(p.Branch); m != nil {
			n, _ := strconv.Atoi(m[1])
			inReview[n] = p.Number
		}
	}
	rows := lines(files[0])
	for _, line := range rows {
		for _, m := range builtNote.FindAllStringSubmatch(line, -1) {
			n, _ := strconv.Atoi(m[1])
			built[n] = true
		}
	}
	var plans []RoadmapPlan
	for _, line := range rows {
		m := roadmapRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		name := m[2]
		if b := boldName.FindStringSubmatch(name); b != nil {
			name = b[1]
		}
		plan := RoadmapPlan{Num: n, Name: name, Status: "planned"}
		deferred, _ := filepath.Glob(filepath.Join(root, fmt.Sprintf("docs/superpowers/plans/*-p%d-deferred.md", n)))
		switch {
		case len(deferred) > 0 || built[n] || finished[n]:
			plan.Status = "done"
			if pr, ok := inReview[n]; ok {
				plan.Status, plan.PR = "review", pr
			}
		case active[n]:
			plan.Status = "active"
		}
		plans = append(plans, plan)
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
			if strings.HasPrefix(m[2], "complete") || strings.Contains(m[2], fmt.Sprintf("Task %d: complete", n)) {
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

// render draws the whole view. rows limits its height (0 means no limit), so it always fits the terminal.
func render(w io.Writer, root string, prs []PR, cols, rows int, clear bool) {
	builds := ReadBuilds(root)
	roadmap := ReadRoadmap(root, builds, prs)
	sections, edited := ReadNow(root)

	var view []string
	add := func(style, s string) { view = append(view, style+clip(s, cols)+reset) }
	gap := func() { view = append(view, "") }
	heading := func(s string) { add(bold, s) }

	branch, commit := gitHead(root)
	add(bold, "Athena · development")
	add(dim, fmt.Sprintf("%s · %s · %s", branch, commit, time.Now().Format("15:04:05")))
	gap()

	if len(sections) == 0 {
		add(dim, "Nothing written yet: put Now and Next lists in "+nowFile)
		gap()
	}
	shownPRs := false
	drawPRs := func() {
		for _, p := range prs {
			add(orange, fmt.Sprintf("  ◆ PR #%d  %s · %s", p.Number, p.Title, p.Checks()))
		}
		shownPRs = true
	}
	for _, sec := range sections {
		now := strings.EqualFold(sec.Title, "now")
		numbered := strings.EqualFold(sec.Title, "next")
		title := sec.Title
		if now && !edited.IsZero() {
			title += "  " + dim + "· updated " + ago(edited) + reset
		}
		view = append(view, bold+title+reset)
		step := 0
		for _, it := range sec.Items {
			switch {
			case it.Done:
				add(green, "  ✓ "+it.Text)
			case now:
				add(orange+bold, "  ▶ "+it.Text)
			case numbered:
				step++
				add("", fmt.Sprintf("  %d. %s", step, it.Text))
			default:
				add("", "  · "+it.Text)
			}
		}
		if now {
			drawPRs()
		}
		gap()
	}
	if !shownPRs && len(prs) > 0 {
		heading("Pull requests")
		drawPRs()
		gap()
	}

	for _, b := range builds {
		if b.Finished() {
			continue
		}
		done := 0
		for _, t := range b.Tasks {
			if t.State == "done" {
				done++
			}
		}
		heading("Building: " + b.PlanName)
		add("", fmt.Sprintf("  %s  %d of %d tasks · ledger %s", bar(done, len(b.Tasks), 24), done, len(b.Tasks), ago(b.Updated)))
		upcoming := 0
		for _, t := range b.Tasks {
			switch {
			case t.State == "active":
				add(orange+bold, fmt.Sprintf("  ▶ %2d  %s", t.Num, t.Title))
				add(orange, "        "+t.Last)
			case t.State == "pending" && upcoming < 3:
				upcoming++
				add(dim, fmt.Sprintf("  · %2d  %s", t.Num, t.Title))
			}
		}
		if b.Final != "" {
			add(orange, "  "+b.Final)
		}
		gap()
	}

	if len(roadmap) > 0 {
		done, review := 0, 0
		for _, p := range roadmap {
			switch p.Status {
			case "done":
				done++
			case "review":
				review++
			}
		}
		summary := fmt.Sprintf("Roadmap  %s  %d of %d built", bar(done+review, len(roadmap), 20), done+review, len(roadmap))
		if review > 0 {
			summary += fmt.Sprintf(" · %d in review", review)
		}
		heading(summary)
		view = append(view, roadmapRows(roadmap, cols)...)
	}

	omitted := 0
	if rows > 0 && len(view) >= rows {
		omitted = len(view) - rows + 2
		view = view[:rows-2]
	}
	if omitted > 0 {
		add(dim, fmt.Sprintf("… %d more lines; make this panel taller", omitted))
	}

	var out strings.Builder
	eol := "\n"
	if clear {
		out.WriteString("\x1b[H")
		eol = "\x1b[K\n"
	}
	for _, l := range view {
		out.WriteString(l + eol)
	}
	if clear {
		out.WriteString("\x1b[J")
	}
	io.WriteString(w, out.String())
}

// roadmapRows lays the plans out in two columns when the terminal is wide enough, one otherwise.
func roadmapRows(plans []RoadmapPlan, cols int) []string {
	cell := func(p RoadmapPlan, width int) (style, text string) {
		text = fmt.Sprintf("  %s %2d  %s", map[string]string{"done": "✓", "review": "◆", "active": "▶"}[p.Status], p.Num, p.Name)
		switch p.Status {
		case "done":
			style = green
		case "review":
			style, text = orange, fmt.Sprintf("  ◆ %2d  %s · PR #%d", p.Num, p.Name, p.PR)
		case "active":
			style = orange + bold
		default:
			style, text = dim, fmt.Sprintf("  · %2d  %s", p.Num, p.Name)
		}
		return style, clip(text, width)
	}
	var rows []string
	if cols < 90 {
		for _, p := range plans {
			style, text := cell(p, cols)
			rows = append(rows, style+text+reset)
		}
		return rows
	}
	width := cols / 2
	half := (len(plans) + 1) / 2
	for i := 0; i < half; i++ {
		style, left := cell(plans[i], width-1)
		row := style + left + reset
		if i+half < len(plans) {
			row += strings.Repeat(" ", width-len([]rune(left)))
			style, right := cell(plans[i+half], cols-width)
			row += style + right + reset
		}
		rows = append(rows, row)
	}
	return rows
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

func itoa(n int) string { return strconv.Itoa(n) }

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
