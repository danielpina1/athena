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
	if err := ob.applied(10); err != nil {
		t.Fatal(err)
	}
	seq, err := ob.append(MethodTurnEnded, []byte(`{}`), time.Now())
	if err != nil || seq != 11 {
		t.Fatalf("seq = %d, %v; want 11", seq, err)
	}
	if err := ob.applied(5); err != nil { // a lower mark changes nothing
		t.Fatal(err)
	}
	rows, err := ob.after(0, 10)
	if err != nil || len(rows) != 1 || rows[0].seq != 11 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
}
