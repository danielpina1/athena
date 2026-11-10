package store

import (
	"testing"
	"time"
)

func TestNodeRoundTrip(t *testing.T) {
	s, _ := openTemp(t)
	if n, err := s.GetNode("n1"); err != nil || n != nil {
		t.Fatalf("missing node = %v, %v", n, err)
	}
	t0 := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	n := Node{ID: "n1", Name: "box", CertSerial: "ab12", CreatedAt: t0, Caps: []byte(`{"claude":true}`),
		CertNotAfter: t0.Add(365 * 24 * time.Hour)}
	if err := s.PutNode(n); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetNode("n1")
	if err != nil || got == nil {
		t.Fatalf("GetNode = %v, %v", got, err)
	}
	if got.Name != "box" || got.CertSerial != "ab12" || !got.CreatedAt.Equal(t0) || !got.LastSeen.IsZero() ||
		got.Draining || got.RevokedAt != nil || got.AppliedFactSeq != 0 || string(got.Caps) != `{"claude":true}` ||
		!got.CertNotAfter.Equal(t0.Add(365*24*time.Hour)) {
		t.Fatalf("node = %+v", got)
	}
	if err := s.TouchNode("n1", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodeDraining("n1", true); err != nil {
		t.Fatal(err)
	}
	if err := s.PutNode(Node{ID: "n2", Name: "alpha", CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	all, err := s.Nodes()
	if err != nil || len(all) != 2 {
		t.Fatalf("Nodes = %v, %v", all, err)
	}
	if all[0].Name != "alpha" || all[1].Name != "box" {
		t.Fatalf("order = %s, %s", all[0].Name, all[1].Name)
	}
	if !all[1].Draining || !all[1].LastSeen.Equal(t0.Add(time.Minute)) || !all[0].CertNotAfter.IsZero() {
		t.Fatalf("node = %+v", all[1])
	}
	if err := s.PutNode(Node{ID: "n3", Name: "box", CreatedAt: t0}); err == nil {
		t.Fatal("duplicate name accepted")
	}
}

func TestAdvanceFactSeqMonotonic(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.PutNode(Node{ID: "n1", Name: "box", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		seq  uint64
		want bool
	}{{3, true}, {5, true}, {5, false}, {4, false}} {
		got, err := s.AdvanceFactSeq("n1", c.seq)
		if err != nil || got != c.want {
			t.Fatalf("AdvanceFactSeq(%d) = %v, %v; want %v", c.seq, got, err, c.want)
		}
	}
	n, _ := s.GetNode("n1")
	if n.AppliedFactSeq != 5 {
		t.Fatalf("mark = %d", n.AppliedFactSeq)
	}
	if ok, err := s.AdvanceFactSeq("ghost", 1); err != nil || ok {
		t.Fatalf("unknown node = %v, %v", ok, err)
	}
}

func TestEnrollTokenKindsDoNotCross(t *testing.T) {
	s, _ := openTemp(t)
	t0 := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	exp := t0.Add(time.Hour)
	if err := s.PutEnrollToken([]byte("a"), EnrollNode, exp); err != nil {
		t.Fatal(err)
	}
	if err := s.PutEnrollToken([]byte("b"), EnrollPasskey, exp); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.UseEnrollToken([]byte("a"), EnrollPasskey, t0); ok {
		t.Fatal("node token accepted as passkey")
	}
	if ok, _ := s.UseEnrollToken([]byte("b"), EnrollNode, t0); ok {
		t.Fatal("passkey token accepted as node")
	}
	if ok, err := s.UseEnrollToken([]byte("a"), EnrollNode, t0); err != nil || !ok {
		t.Fatalf("node token = %v, %v", ok, err)
	}
	if ok, err := s.UseEnrollToken([]byte("b"), EnrollPasskey, t0); err != nil || !ok {
		t.Fatalf("passkey token = %v, %v", ok, err)
	}
}

func TestRevokedNodeKeepsRow(t *testing.T) {
	s, _ := openTemp(t)
	t0 := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	if err := s.PutNode(Node{ID: "n1", Name: "box", CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeNode("n1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	n, err := s.GetNode("n1")
	if err != nil || n == nil || n.RevokedAt == nil || !n.RevokedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("node = %+v, %v", n, err)
	}
	all, _ := s.Nodes()
	if len(all) != 1 {
		t.Fatalf("Nodes = %d", len(all))
	}
}

func TestSetNodeCapsKeepsMark(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.PutNode(Node{ID: "n1", Name: "box", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceFactSeq("n1", 7); err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodeCaps("n1", []byte("caps")); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetNode("n1")
	if err != nil || got == nil || string(got.Caps) != "caps" || got.AppliedFactSeq != 7 {
		t.Fatalf("node = %+v, %v", got, err)
	}
}
