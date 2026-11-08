package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) string {
	root := t.TempDir()
	write(t, root, "docs/superpowers/plans/2026-11-03-athena-roadmap.md", `| # | Plan | Ends with | Spec |
| --- | --- | --- | --- |
| 1 | **Foundation** (`+"`x.md`"+`) | a | b |
| 2 | Router | a | b |
| 3 | Dashboard UI | a | b |
`)
	write(t, root, "docs/superpowers/plans/2026-11-03-athena-p1-deferred.md", "done\n")
	write(t, root, "docs/superpowers/plans/2026-11-04-athena-p2-router.md", `# Athena Plan 2: Router Implementation Plan

### Task 1: Store
### Task 2: Engine
### Task 3: Wiring
`)
	write(t, root, ".superpowers/sdd/2026-11-04-athena-p2-router/progress.md", `# SDD ledger — plan: docs/superpowers/plans/2026-11-04-athena-p2-router.md

- Ruling R1: something
Task 1: dispatched (BASE abc)
Task 1: fix round 1/5 (x)
Task 1: complete (commits a..b, review clean)
Task 2: dispatched (BASE def)
Task 2: review needs fixes → fix round 1
- Ruling R2: other
`)
	return root
}

func TestReadBuildsTaskStates(t *testing.T) {
	builds := ReadBuilds(fixture(t))
	if len(builds) != 1 {
		t.Fatalf("builds = %d, want 1", len(builds))
	}
	b := builds[0]
	if b.PlanName != "Athena Plan 2: Router" || b.Rulings != 2 || len(b.Tasks) != 3 {
		t.Fatalf("build = %+v", b)
	}
	want := []string{"done", "active", "pending"}
	for i, s := range want {
		if b.Tasks[i].State != s {
			t.Errorf("task %d state %q, want %q", i+1, b.Tasks[i].State, s)
		}
	}
	if b.Tasks[0].FixRounds != 1 || b.Tasks[1].FixRounds != 0 {
		t.Errorf("fix rounds %d/%d, want 1/0", b.Tasks[0].FixRounds, b.Tasks[1].FixRounds)
	}
	if !strings.HasPrefix(b.Tasks[1].Last, "review needs fixes") {
		t.Errorf("task 2 last = %q", b.Tasks[1].Last)
	}
}

func TestReadShowsWorkOutsideTasks(t *testing.T) {
	root := fixture(t)
	write(t, root, ".superpowers/sdd/2026-11-04-athena-p2-router/progress.md", `# SDD ledger — plan: docs/superpowers/plans/2026-11-04-athena-p2-router.md

Task 1: complete (commits a..b)
| 1 | table row |
- Ruling R1: something
  indented detail
Session view: implementer dispatched
`)
	b := ReadBuilds(root)[0]
	want := []string{"Task 1: complete (commits a..b)", "Session view: implementer dispatched"}
	if strings.Join(b.Recent, "\n") != strings.Join(want, "\n") {
		t.Fatalf("recent = %q, want %q", b.Recent, want)
	}
}

func TestReadRoadmapStatuses(t *testing.T) {
	root := fixture(t)
	plans := ReadRoadmap(root, ReadBuilds(root), nil)
	got := map[int]string{}
	for _, p := range plans {
		got[p.Num] = p.Status
	}
	if got[1] != "done" || got[2] != "active" || got[3] != "planned" || plans[0].Name != "Foundation" {
		t.Fatalf("roadmap = %+v", plans)
	}
}

func TestRenderClipsLongLines(t *testing.T) {
	var sb strings.Builder
	render(&sb, fixture(t), nil, 40, 0, false)
	for _, l := range strings.Split(sb.String(), "\n") {
		plain := l
		for _, esc := range []string{reset, bold, dim, orange, green} {
			plain = strings.ReplaceAll(plain, esc, "")
		}
		if n := len([]rune(plain)); n > 40 {
			t.Fatalf("line of %d runes: %q", n, plain)
		}
	}
}

func TestRoadmapBuiltNoteAndOpenPR(t *testing.T) {
	root := fixture(t)
	write(t, root, "docs/superpowers/plans/2026-11-03-athena-roadmap.md", `| # | Plan | Ends with | Spec |
| --- | --- | --- | --- |
| 3 | Dashboard UI | a | b |
| 4 | Brakes | a | b |

**Plan 3 is built** (branch x). **Plan 4 is built** too.
`)
	plans := ReadRoadmap(root, nil, []PR{{Number: 12, Branch: "p4-brakes"}, {Number: 13, Branch: "unrelated"}})
	if plans[0].Status != "done" || plans[1].Status != "review" || plans[1].PR != 12 {
		t.Fatalf("roadmap = %+v", plans)
	}
}

func TestCompleteInsideALaterSentenceFinishesTheTask(t *testing.T) {
	root := fixture(t)
	write(t, root, ".superpowers/sdd/2026-11-04-athena-p2-router/progress.md", `# SDD ledger — plan: docs/superpowers/plans/2026-11-04-athena-p2-router.md

Task 1: re-review CLEAN. Task 1: complete (commits a..b)
Task 2: complete
Task 3: complete
Final review: dispatched
`)
	b := ReadBuilds(root)[0]
	if b.Tasks[0].State != "done" {
		t.Fatalf("task 1 state = %q", b.Tasks[0].State)
	}
	if b.Finished() {
		t.Fatal("finished while the final review is still running")
	}
	write(t, root, ".superpowers/sdd/2026-11-04-athena-p2-router/progress.md", `# SDD ledger — plan: docs/superpowers/plans/2026-11-04-athena-p2-router.md

Task 1: complete
Task 2: complete
Task 3: complete
Final re-review: all addressed
`)
	if !ReadBuilds(root)[0].Finished() {
		t.Fatal("not finished after the final re-review")
	}
}

func TestReadNow(t *testing.T) {
	root := fixture(t)
	write(t, root, nowFile, `# Now
- Hardening session running

# Next
- [x] Open the PR
- [ ] Merge it
stray text
`)
	sections, modified := ReadNow(root)
	if modified.IsZero() || len(sections) != 2 || sections[0].Title != "Now" {
		t.Fatalf("sections = %+v", sections)
	}
	next := sections[1].Items
	if len(next) != 2 || !next[0].Done || next[0].Text != "Open the PR" || next[1].Done || next[1].Text != "Merge it" {
		t.Fatalf("next = %+v", next)
	}
}

func TestParsePRsCountsChecks(t *testing.T) {
	prs, err := parsePRs([]byte(`[{"number":12,"title":"T","headRefName":"p10-x","statusCheckRollup":[
		{"conclusion":"SUCCESS"},{"conclusion":"FAILURE"},{"state":"PENDING"},{"conclusion":"","state":""}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	p := prs[0]
	if p.Passed != 1 || p.Failed != 1 || p.Other != 2 || p.Checks() != "1 check failing" {
		t.Fatalf("pr = %+v checks %q", p, p.Checks())
	}
	if (PR{}).Checks() != "no checks" || (PR{Passed: 2}).Checks() != "2 checks passed" {
		t.Fatalf("checks wording")
	}
}

func TestRenderFitsTheHeightAndPutsPRsUnderNow(t *testing.T) {
	root := fixture(t)
	write(t, root, nowFile, "# Now\n- Working\n\n# Next\n- One\n- Two\n")
	var sb strings.Builder
	render(&sb, root, []PR{{Number: 12, Title: "Brakes"}}, 100, 12, false)
	got := strings.TrimRight(sb.String(), "\n")
	if n := len(strings.Split(got, "\n")); n > 11 {
		t.Fatalf("%d lines for 12 rows:\n%s", n, got)
	}
	if !strings.Contains(got, "more lines") {
		t.Fatalf("no overflow note:\n%s", got)
	}
	now, pr, next := strings.Index(got, "Now"), strings.Index(got, "PR #12"), strings.Index(got, "Next")
	if !(now < pr && pr < next) {
		t.Fatalf("order now=%d pr=%d next=%d:\n%s", now, pr, next, got)
	}
}
