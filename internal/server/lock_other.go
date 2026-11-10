//go:build !unix

package server

// lockStateDir takes no lock where flock is missing: a second node on one state directory is not refused there.
func lockStateDir(string) (func(), error) { return func() {}, nil }
