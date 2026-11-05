package codex

import "testing"

func TestParseVersionAndSupported(t *testing.T) {
	for _, c := range []struct {
		out, want string
		err       bool
	}{
		{"codex-cli 0.160.1\n", "0.160.1", false},
		{"  codex-cli 1.2.30  ", "1.2.30", false},
		{"WARNING: proxy\ncodex-cli 0.161.0\n", "0.161.0", false},
		{"claude 2.1.280", "", true},
		{"", "", true},
		{"codex-cli v1", "", true},
	} {
		got, err := ParseVersion(c.out)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("ParseVersion(%q) = %q, %v; want %q (error %v)", c.out, got, err, c.want, c.err)
		}
	}
	for _, c := range []struct {
		v          string
		ok, tested bool
	}{
		{"0.159.9", false, true},
		{MinVersion, true, true},
		{VerifiedVersion, true, true},
		{"0.160.2", true, false},
		{"0.161.0", true, false},
		{"1.0.0", true, false},
		{"0.9.0", false, true},
		{"garbage", false, false},
		{"0.160", false, false},
		{"", false, false},
	} {
		ok, tested := Supported(c.v)
		if ok != c.ok || tested != c.tested {
			t.Errorf("Supported(%q) = %v, %v; want %v, %v", c.v, ok, tested, c.ok, c.tested)
		}
	}
}
