package store

import (
	"reflect"
	"testing"
	"time"

	"github.com/danielpina1/athena/internal/core"
)

func TestBrakeSurvivesRestart(t *testing.T) {
	s, path := openTemp(t)
	late := core.Brake{TriggerID: "github:issuecomment:2", Repo: "acme/web", Thread: "github:acme/web#2", At: t0.Add(time.Minute)}
	early := core.Brake{TriggerID: "github:issuecomment:1", Repo: "acme/web", Thread: "github:acme/web#1", At: t0}
	inTx(t, s, func(tx core.Tx) error {
		if err := tx.PutBrake(late); err != nil {
			return err
		}
		return tx.PutBrake(early)
	})
	// An upsert of the same trigger replaces its row.
	moved := late
	moved.Thread = "github:acme/web#5"
	moved.At = t0.Add(2 * time.Minute)
	inTx(t, s, func(tx core.Tx) error { return tx.PutBrake(moved) })
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var got []core.Brake
	inTx(t, s2, func(tx core.Tx) (err error) {
		got, err = tx.Brakes()
		return err
	})
	if want := []core.Brake{early, moved}; !reflect.DeepEqual(got, want) {
		t.Fatalf("brakes = %+v, want %+v", got, want)
	}
	inTx(t, s2, func(tx core.Tx) error {
		if err := tx.DeleteBrake("never-there"); err != nil {
			return err
		}
		return tx.DeleteBrake(early.TriggerID)
	})
	inTx(t, s2, func(tx core.Tx) (err error) { got, err = tx.Brakes(); return err })
	if want := []core.Brake{moved}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after delete brakes = %+v, want %+v", got, want)
	}
}
