//go:build !unix

package server

import "runtime/metrics"

// processRSS approximates the resident set size by the memory the Go runtime has mapped, where getrusage does not
// exist.
func processRSS() uint64 {
	s := []metrics.Sample{{Name: "/memory/classes/total:bytes"}}
	metrics.Read(s)
	if s[0].Value.Kind() == metrics.KindUint64 {
		return s[0].Value.Uint64()
	}
	return 0
}
