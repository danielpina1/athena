package github

import (
	"slices"
	"testing"
)

func TestParseReferences(t *testing.T) {
	text := "Fixes #12, see Other/Repo#3 and https://github.com/O/R/pull/7 or https://github.com/o/r/issues/12; " +
		"not #L10, not foo#4x, not issue#5 and not https://github.com/o/r/pull/7/files#diff-1 twice"
	got := ParseReferences(text, "github:o/r#1")
	want := []string{"github:o/r#12", "github:other/repo#3", "github:o/r#7"}
	if !slices.Equal(got, want) {
		t.Fatalf("references = %q, want %q", got, want)
	}
	if got := ParseReferences("see #4", "github:o/r@main"); !slices.Equal(got, []string{"github:o/r#4"}) {
		t.Fatalf("bare reference on a branch thread = %q", got)
	}
	if got := ParseReferences("see #4", "feishu:x"); len(got) != 0 {
		t.Fatalf("a bare reference without a repository = %q", got)
	}
	var s Source
	if got := s.References("#2", "github:o/r#1"); !slices.Equal(got, []string{"github:o/r#2"}) {
		t.Fatalf("Source.References = %q", got)
	}
}
