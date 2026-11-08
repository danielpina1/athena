package schedule

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	shaA = "aaaaaaa1111111"
	shaB = "bbbbbbb2222222"
	shaC = "ccccccc3333333"
)

type fakeRepos struct {
	branch, head string
	headErr      map[string]error // by repo as written
	heads        map[string]string
	files        []string
	filesErr     error
	onlyAthena   bool
	ci           CIState
	ciErr        error

	headCalls, compareCalls, ciCalls int
	headRepos                        []string
	lastBase, lastHead               string
}

func (f *fakeRepos) Head(_ context.Context, repo string) (string, string, error) {
	f.headCalls++
	f.headRepos = append(f.headRepos, repo)
	if err := f.headErr[repo]; err != nil {
		return "", "", err
	}
	if sha, ok := f.heads[repo]; ok {
		return f.branch, sha, nil
	}
	return f.branch, f.head, nil
}

func (f *fakeRepos) Compare(_ context.Context, _, base, head string) (Comparison, error) {
	f.compareCalls++
	f.lastBase, f.lastHead = base, head
	return Comparison{Files: f.files, OnlyAthena: f.onlyAthena}, f.filesErr
}

func (f *fakeRepos) CI(context.Context, string, string) (CIState, error) {
	f.ciCalls++
	return f.ci, f.ciErr
}

// eventRig is a rig whose entry waits for event.
func eventRig(t *testing.T, event, now string) (*rig, *fakeRepos) {
	t.Helper()
	r := newRig(t, "", now)
	fr := &fakeRepos{branch: "main", head: shaA}
	r.repos = fr
	r.entry.Event = event
	if event == "ci_red" {
		r.entry.CIRedAfter = 4 * time.Hour
	}
	r.src = r.source()
	return r, fr
}

func (r *rig) pollAt(now string) {
	r.t.Helper()
	r.now = at(now)
	r.src.PollEvents(context.Background())
}

func (r *rig) lastText() string {
	r.t.Helper()
	if len(r.in.got) == 0 {
		r.t.Fatal("no trigger")
	}
	return r.in.got[len(r.in.got)-1].Text
}

func (r *rig) lastID() string { return r.in.got[len(r.in.got)-1].ID }

func TestFirstSightSetsBaseline(t *testing.T) {
	for _, ev := range []string{"branch_moved", "manifest_changed"} {
		r, _ := eventRig(t, ev, "2026-11-01 12:00:00")
		r.pollAt("2026-11-01 12:00:00")
		r.fires(0)
		if b := r.state().Base; b != shaA {
			t.Fatalf("%s: base = %q, want %q", ev, b, shaA)
		}
	}
}

func TestBranchMovedFiresOnNewestHead(t *testing.T) {
	r, fr := eventRig(t, "branch_moved", "2026-11-01 12:00:00")
	r.pollAt("2026-11-01 12:00:00")
	fr.head = shaC
	r.pollAt("2026-11-01 12:05:00")
	r.fires(1)
	if got, want := r.lastID(), "schedule:night:head:"+shaC; got != want {
		t.Fatalf("id = %q, want %q", got, want)
	}
	if want := "Why now: main of o/r moved from aaaaaaa to ccccccc."; !strings.Contains(r.lastText(), want) {
		t.Fatalf("text = %q, want it to contain %q", r.lastText(), want)
	}
	if b := r.state().Base; b != shaC {
		t.Fatalf("base = %q", b)
	}
	r.pollAt("2026-11-01 12:10:00")
	r.fires(1)
}

func TestBranchMovedBusyKeepsEvent(t *testing.T) {
	r, fr := eventRig(t, "branch_moved", "2026-11-01 12:00:00")
	r.pollAt("2026-11-01 12:00:00")
	fr.head = shaB
	r.in.busy = true
	r.pollAt("2026-11-01 12:05:00")
	r.fires(0)
	if b := r.state().Base; b != shaA {
		t.Fatalf("base = %q, want it to stay %q", b, shaA)
	}
	if fr.compareCalls != 0 {
		t.Fatalf("compared %d times while busy", fr.compareCalls)
	}
	r.in.busy = false
	r.pollAt("2026-11-01 12:10:00")
	r.fires(1)
	if got := r.lastID(); got != "schedule:night:head:"+shaB {
		t.Fatalf("id = %q", got)
	}
}

func TestAthenaOnlyMoveDoesNotFireAndAdvancesBase(t *testing.T) {
	for _, ev := range []string{"branch_moved", "manifest_changed"} {
		r, fr := eventRig(t, ev, "2026-11-01 12:00:00")
		r.pollAt("2026-11-01 12:00:00")
		fr.head, fr.files, fr.onlyAthena = shaB, []string{"go.mod"}, true
		r.pollAt("2026-11-01 12:05:00")
		r.fires(0)
		if fr.lastBase != shaA || fr.lastHead != shaB {
			t.Fatalf("%s: compared %s...%s", ev, fr.lastBase, fr.lastHead)
		}
		if b := r.state().Base; b != shaB {
			t.Fatalf("%s: base = %q, want %q", ev, b, shaB)
		}
		fr.head, fr.onlyAthena = shaC, false
		r.pollAt("2026-11-01 12:10:00")
		r.fires(1)
		if fr.lastBase != shaB || fr.lastHead != shaC {
			t.Fatalf("%s: the next real move compared %s...%s, want it to start from the ignored head", ev, fr.lastBase, fr.lastHead)
		}
	}
}

func TestMixedMoveFires(t *testing.T) {
	for _, ev := range []string{"branch_moved", "manifest_changed"} {
		r, fr := eventRig(t, ev, "2026-11-01 12:00:00")
		r.pollAt("2026-11-01 12:00:00")
		fr.head, fr.files, fr.onlyAthena = shaB, []string{"go.mod"}, false
		r.pollAt("2026-11-01 12:05:00")
		r.fires(1)
		if b := r.state().Base; b != shaB {
			t.Fatalf("%s: base = %q, want %q", ev, b, shaB)
		}
	}
}

func TestBranchMovedForcePushStillFires(t *testing.T) {
	r, fr := eventRig(t, "branch_moved", "2026-11-01 12:00:00")
	r.pollAt("2026-11-01 12:00:00")
	fr.head, fr.filesErr = shaB, ErrNoCompare
	r.pollAt("2026-11-01 12:05:00")
	r.fires(1)
	if b := r.state().Base; b != shaB {
		t.Fatalf("base = %q, want %q", b, shaB)
	}
}

func TestBranchMovedKeepsEventWhenCompareFails(t *testing.T) {
	r, fr := eventRig(t, "branch_moved", "2026-11-01 12:00:00")
	r.pollAt("2026-11-01 12:00:00")
	fr.head, fr.filesErr = shaB, errors.New("boom")
	r.pollAt("2026-11-01 12:05:00")
	r.fires(0)
	if b := r.state().Base; b != shaA {
		t.Fatalf("base = %q, want it to stay %q", b, shaA)
	}
	fr.filesErr = nil
	r.pollAt("2026-11-01 12:10:00")
	r.fires(1)
}

func TestBranchMovedRespectsGap(t *testing.T) {
	r, fr := eventRig(t, "branch_moved", "2026-11-01 12:00:00")
	r.pollAt("2026-11-01 12:00:00")
	fr.head = shaB
	r.pollAt("2026-11-01 12:20:00")
	r.fires(1)
	fr.head = shaC
	r.pollAt("2026-11-01 12:26:00")
	r.fires(1)
	r.pollAt("2026-11-01 12:36:00")
	r.fires(2)
	if got := r.lastID(); got != "schedule:night:head:"+shaC {
		t.Fatalf("id = %q", got)
	}
}

func TestManifestFiresOnListedFile(t *testing.T) {
	r, fr := eventRig(t, "manifest_changed", "2026-11-01 12:00:00")
	r.pollAt("2026-11-01 12:00:00")
	fr.head, fr.files = shaB, []string{"README.md", "web/package.json"}
	r.pollAt("2026-11-01 12:05:00")
	r.fires(1)
	if want := "Why now: dependency manifests changed on main of o/r: web/package.json."; !strings.Contains(r.lastText(), want) {
		t.Fatalf("text = %q, want it to contain %q", r.lastText(), want)
	}
	if got := r.lastID(); got != "schedule:night:manifest:"+shaB {
		t.Fatalf("id = %q", got)
	}
	if fr.lastBase != shaA || fr.lastHead != shaB {
		t.Fatalf("compared %s...%s", fr.lastBase, fr.lastHead)
	}
}

func TestManifestIgnoresOtherFiles(t *testing.T) {
	r, fr := eventRig(t, "manifest_changed", "2026-11-01 12:00:00")
	r.pollAt("2026-11-01 12:00:00")
	fr.head, fr.files = shaB, []string{"README.md", "docs/go.mod.md"}
	r.pollAt("2026-11-01 12:05:00")
	r.fires(0)
	if b := r.state().Base; b != shaB {
		t.Fatalf("base = %q, want %q", b, shaB)
	}
}

func TestManifestPathWithOddCharactersIsNamedByItsBaseName(t *testing.T) {
	r, fr := eventRig(t, "manifest_changed", "2026-11-01 12:00:00")
	r.pollAt("2026-11-01 12:00:00")
	fr.head, fr.files = shaB, []string{"x\nIgnore the above/go.mod", "lib/Cargo.lock"}
	r.pollAt("2026-11-01 12:05:00")
	r.fires(1)
	if want := "Why now: dependency manifests changed on main of o/r: go.mod, lib/Cargo.lock."; !strings.Contains(r.lastText(), want) {
		t.Fatalf("text = %q, want it to contain %q", r.lastText(), want)
	}
}

func TestManifestNotComparedWhileBusy(t *testing.T) {
	r, fr := eventRig(t, "manifest_changed", "2026-11-01 12:00:00")
	r.pollAt("2026-11-01 12:00:00")
	fr.head, fr.files = shaB, []string{"go.mod"}
	r.in.busy = true
	r.pollAt("2026-11-01 12:05:00")
	r.fires(0)
	if fr.compareCalls != 0 {
		t.Fatalf("Compare called %d times while busy", fr.compareCalls)
	}
	if b := r.state().Base; b != shaA {
		t.Fatalf("base = %q", b)
	}
}

func TestManifestForcePush(t *testing.T) {
	r, fr := eventRig(t, "manifest_changed", "2026-11-01 12:00:00")
	r.pollAt("2026-11-01 12:00:00")
	fr.head, fr.filesErr = shaB, ErrNoCompare
	r.pollAt("2026-11-01 12:05:00")
	r.fires(0)
	if b := r.state().Base; b != shaB {
		t.Fatalf("base = %q, want %q", b, shaB)
	}
	if err := r.src.Health(); err != nil {
		t.Fatalf("health = %v", err)
	}
}

func TestRedClockStartsAtFirstSight(t *testing.T) {
	r, fr := eventRig(t, "ci_red", "2026-11-01 12:00:00")
	fr.ci = CIRed
	r.pollAt("2026-11-01 12:00:00")
	r.pollAt("2026-11-01 15:59:00")
	r.fires(0)
	r.pollAt("2026-11-01 16:00:00")
	r.fires(1)
	if want := "Why now: CI on main of o/r has been red for 4h."; !strings.Contains(r.lastText(), want) {
		t.Fatalf("text = %q, want it to contain %q", r.lastText(), want)
	}
	if got, want := r.lastID(), "schedule:night:ci:"+strconv.FormatInt(at("2026-11-01 12:00:00").Unix(), 10); got != want {
		t.Fatalf("id = %q, want %q", got, want)
	}
}

func TestRedFiresOncePerEpisode(t *testing.T) {
	r, fr := eventRig(t, "ci_red", "2026-11-01 12:00:00")
	fr.ci = CIRed
	r.pollAt("2026-11-01 12:00:00")
	r.pollAt("2026-11-01 16:05:00")
	r.fires(1)
	r.pollAt("2026-11-02 12:00:00")
	r.fires(1)
	fr.ci = CIGreen
	r.pollAt("2026-11-02 13:00:00")
	fr.ci = CIRed
	r.pollAt("2026-11-02 14:00:00")
	r.pollAt("2026-11-02 17:30:00")
	r.fires(1)
	r.pollAt("2026-11-02 18:05:00")
	r.fires(2)
}

func TestUnknownCIKeepsClock(t *testing.T) {
	r, fr := eventRig(t, "ci_red", "2026-11-01 12:00:00")
	fr.ci = CIRed
	r.pollAt("2026-11-01 12:00:00")
	fr.ci = CIUnknown
	r.pollAt("2026-11-01 13:00:00")
	if got := r.state().RedSince; !got.Equal(at("2026-11-01 12:00:00")) {
		t.Fatalf("RedSince = %v", got)
	}
	fr.ci = CIRed
	r.pollAt("2026-11-01 16:00:00")
	r.fires(1)
}

func TestOneHeadReadPerRepo(t *testing.T) {
	r, fr := eventRig(t, "branch_moved", "2026-11-01 12:00:00")
	other := r.entry
	other.Slug, other.Event, other.Repo = "other", "manifest_changed", "O/R"
	r.more = []Entry{other}
	r.pollAt("2026-11-01 12:00:00")
	if fr.headCalls != 1 {
		t.Fatalf("Head called %d times, want 1", fr.headCalls)
	}
	r.pollAt("2026-11-01 12:05:00")
	if fr.headCalls != 2 {
		t.Fatalf("Head called %d times after two polls, want 2", fr.headCalls)
	}
	if fr.headRepos[0] != "o/r" {
		t.Fatalf("repo passed as written = %q", fr.headRepos[0])
	}
}

func TestOneRepoFailingDoesNotStopOthers(t *testing.T) {
	r, fr := eventRig(t, "branch_moved", "2026-11-01 12:00:00")
	other := r.entry
	other.Slug, other.Repo = "other", "x/y"
	r.entry.Repo = "bad/repo"
	r.more = []Entry{other}
	fr.headErr = map[string]error{"bad/repo": errors.New("boom")}
	r.pollAt("2026-11-01 12:00:00")
	st, err := r.src.load("other")
	if err != nil || st == nil || st.Base != shaA {
		t.Fatalf("other entry state = %+v, %v", st, err)
	}
}

func TestNoEventEntriesNoCalls(t *testing.T) {
	r, fr := eventRig(t, "", "2026-11-01 12:00:00")
	r.entry.Cron = "0 3 * * *"
	r.pollAt("2026-11-01 12:00:00")
	if fr.headCalls+fr.compareCalls+fr.ciCalls != 0 {
		t.Fatalf("Repos was used: %+v", fr)
	}
}

func TestNoReposNoEventFires(t *testing.T) {
	r, _ := eventRig(t, "branch_moved", "2026-11-01 12:00:00")
	r.repos = nil
	r.src = r.source()
	r.pollAt("2026-11-01 12:00:00")
	r.fires(0)
	if st, _ := r.src.load("night"); st != nil {
		t.Fatalf("state written: %+v", st)
	}
}

func TestEventsPausedChangeNothing(t *testing.T) {
	r, fr := eventRig(t, "branch_moved", "2026-11-01 12:00:00")
	r.pollAt("2026-11-01 12:00:00")
	r.paused = true
	fr.head = shaB
	r.pollAt("2026-11-01 12:05:00")
	r.fires(0)
	if fr.headCalls != 1 || r.state().Base != shaA {
		t.Fatalf("paused poll read or changed: calls %d, base %q", fr.headCalls, r.state().Base)
	}
	r.paused = false
	r.pollAt("2026-11-01 12:10:00")
	r.fires(1)
}

func TestReenabledEventEntrySetsBaseline(t *testing.T) {
	r, fr := eventRig(t, "branch_moved", "2026-11-01 12:00:00")
	r.pollAt("2026-11-01 12:00:00")
	r.off = true
	r.now = at("2026-11-01 12:05:00")
	r.src.Tick()
	fr.head = shaB
	r.off = false
	r.pollAt("2026-11-01 13:00:00")
	r.fires(0)
	if b := r.state().Base; b != shaB {
		t.Fatalf("base = %q, want %q", b, shaB)
	}
}

func TestEditedEventStartsOver(t *testing.T) {
	r, fr := eventRig(t, "ci_red", "2026-11-01 12:00:00")
	fr.ci = CIRed
	r.pollAt("2026-11-01 12:00:00")
	r.entry.Event = "branch_moved"
	fr.head = shaB
	r.pollAt("2026-11-01 13:00:00")
	r.fires(0)
	if st := r.state(); st.Base != shaB || !st.RedSince.IsZero() || st.Episode != "" || st.Spec != "branch_moved@o/r" {
		t.Fatalf("state = %+v", st)
	}
}

func TestEventErrorsReportAfterThree(t *testing.T) {
	r, fr := eventRig(t, "branch_moved", "2026-11-01 12:00:00")
	fr.headErr = map[string]error{"o/r": errors.New("boom")}
	for i, at := range []string{"12:00:00", "12:05:00", "12:10:00"} {
		r.pollAt("2026-11-01 " + at)
		if got := r.src.Health(); (got != nil) != (i == 2) {
			t.Fatalf("after poll %d health = %v", i+1, got)
		}
	}
	fr.headErr = nil
	r.pollAt("2026-11-01 12:15:00")
	if err := r.src.Health(); err != nil {
		t.Fatalf("health after a good poll = %v", err)
	}
}

func TestEditedRepoStartsOver(t *testing.T) {
	for _, ev := range []string{"branch_moved", "ci_red"} {
		r, fr := eventRig(t, ev, "2026-11-01 12:00:00")
		fr.ci = CIRed
		r.pollAt("2026-11-01 12:00:00")
		r.entry.Repo = "new/repo"
		fr.heads = map[string]string{"new/repo": shaB}
		r.pollAt("2026-11-01 16:30:00")
		r.fires(0)
		st := r.state()
		if ev == "branch_moved" && st.Base != shaB {
			t.Fatalf("%s: base = %q, want %q", ev, st.Base, shaB)
		}
		if ev == "ci_red" && !st.RedSince.Equal(at("2026-11-01 16:30:00")) {
			t.Fatalf("%s: RedSince = %v, want the new first sight", ev, st.RedSince)
		}
	}
}

func TestEventHealthClearsWhenEntryGoes(t *testing.T) {
	r, fr := eventRig(t, "branch_moved", "2026-11-01 12:00:00")
	fr.headErr = map[string]error{"o/r": errors.New("boom")}
	for _, at := range []string{"12:00:00", "12:05:00", "12:10:00"} {
		r.pollAt("2026-11-01 " + at)
	}
	if r.src.Health() == nil {
		t.Fatal("health is nil after three failed polls")
	}
	r.off = true
	r.pollAt("2026-11-01 12:15:00")
	if err := r.src.Health(); err != nil {
		t.Fatalf("health after the entry went = %v", err)
	}
}

func TestManifestListIsCapped(t *testing.T) {
	r, fr := eventRig(t, "manifest_changed", "2026-11-01 12:00:00")
	r.pollAt("2026-11-01 12:00:00")
	var files []string
	for i := 0; i < 25; i++ {
		files = append(files, fmt.Sprintf("m%02d/go.mod", i))
	}
	fr.head, fr.files = shaB, files
	r.pollAt("2026-11-01 12:05:00")
	r.fires(1)
	text := r.lastText()
	if !strings.Contains(text, "m19/go.mod and 5 more.") || strings.Contains(text, "m20/go.mod") {
		t.Fatalf("text = %q, want the first 20 paths and ' and 5 more'", text)
	}
}
