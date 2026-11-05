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
	plans := ReadRoadmap(root, ReadBuilds(root))
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
	render(&sb, fixture(t), 40, false)
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
