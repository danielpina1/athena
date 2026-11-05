// Package codex drives the Codex CLI (`codex app-server`) as an Athena runtime. This file and rpc.go and translate.go
// are the protocol layer: the version gate, the JSON-RPC client over the server's stdio, and the translation of its
// messages into rt events.
package codex

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	// VerifiedVersion is the Codex version Athena's use of it was last checked against; a newer one is Supported but
	// not tested.
	VerifiedVersion = "0.160.1"
	// MinVersion is the oldest Codex version Athena runs.
	MinVersion = "0.160.0"
)

var versionRe = regexp.MustCompile(`codex-cli\s+(\d+\.\d+\.\d+)`)

// ParseVersion returns X.Y.Z from the output of `codex --version` ("codex-cli X.Y.Z").
func ParseVersion(out string) (string, error) {
	m := versionRe.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("codex version: no \"codex-cli X.Y.Z\" in %q", strings.TrimSpace(out))
	}
	return m[1], nil
}

// Supported reports whether Athena runs Codex version v (ok: it is at least MinVersion) and whether that version is
// one Athena was verified against (tested: it is not newer than VerifiedVersion). A string that is not X.Y.Z is not
// supported.
func Supported(v string) (ok, tested bool) {
	n, good := parseTriple(v)
	if !good {
		return false, false
	}
	min, _ := parseTriple(MinVersion)
	ver, _ := parseTriple(VerifiedVersion)
	return !less(n, min), !less(ver, n)
}

func parseTriple(v string) ([3]int, bool) {
	var t [3]int
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return t, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return t, false
		}
		t[i] = n
	}
	return t, true
}

func less(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
