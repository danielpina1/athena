package nodelink

import (
	"testing"
	"time"
)

// A server that applied more than this outbox ever numbered (node.db lost) must not see the next facts as old.
func TestOutboxNumbersAfterTheServersMark(t *testing.T) {
	ob, err := openOutbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ob.close()
	if _, err := ob.applied(10); err != nil {
		t.Fatal(err)
	}
	seq, err := ob.append(MethodTurnEnded, []byte(`{}`), time.Now())
	if err != nil || seq != 11 {
		t.Fatalf("seq = %d, %v; want 11", seq, err)
	}
	if err := ob.markSent(10, 11); err != nil {
		t.Fatal(err)
	}
	if _, err := ob.applied(5); err != nil { // a lower mark changes nothing
		t.Fatal(err)
	}
	rows, err := ob.after(0, 10)
	if err != nil || len(rows) != 1 || rows[0].seq != 11 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
}

// On a fresh node.db, facts made before the first Welcome are numbered from 1 while the server's mark counts the old
// node.db's facts: the ones never sent are numbered again above the mark, in order, instead of being deleted unsent.
// Facts that went on a link are deleted as before.
func TestOutboxRenumbersUnsentFactsBelowTheMark(t *testing.T) {
	ob, err := openOutbox(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ob.close()
	for i, m := range []string{MethodAssignmentStarted, MethodRecordUsage, MethodTurnEnded} {
		if seq, err := ob.append(m, []byte(`{}`), time.Now()); err != nil || seq != uint64(i+1) {
			t.Fatalf("seq = %d, %v", seq, err)
		}
	}
	if err := ob.markSent(0, 1); err != nil { // the first went on a link (and so counts as applied)
		t.Fatal(err)
	}
	moved, err := ob.applied(2)
	if err != nil {
		t.Fatal(err)
	}
	// Above every row (3 is taken), in order.
	if len(moved) != 2 || moved[2] != 4 || moved[3] != 5 {
		t.Fatalf("renumbered = %v, want 2→4, 3→5", moved)
	}
	rows, err := ob.after(2, 10)
	if err != nil || len(rows) != 2 || rows[0].seq != 4 || rows[0].method != MethodRecordUsage ||
		rows[1].seq != 5 || rows[1].method != MethodTurnEnded {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if all, _ := ob.after(0, 10); len(all) != 2 {
		t.Fatalf("the sent fact was kept: %+v", all)
	}
	if seq, err := ob.append(MethodTurnEnded, []byte(`{}`), time.Now()); err != nil || seq != 6 {
		t.Fatalf("next seq = %d, %v; want 6", seq, err)
	}
	// Once sent, rows at or below a later mark are deleted, not moved.
	if err := ob.markSent(2, 6); err != nil {
		t.Fatal(err)
	}
	if moved, err := ob.applied(6); err != nil || len(moved) != 0 {
		t.Fatalf("renumbered = %v, %v", moved, err)
	}
	if all, _ := ob.after(0, 10); len(all) != 0 {
		t.Fatalf("rows left = %+v", all)
	}
}
