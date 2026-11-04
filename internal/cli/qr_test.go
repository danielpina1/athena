package cli

import (
	"bytes"
	"strings"
	"testing"
)

// decodeQR reads a symbol the way a scanner does, from the matrix alone: format bits, unmasking, the zigzag,
// de-interleaving, Reed-Solomon checks, then the byte-mode payload. It uses none of the encoder's choices except the
// function-module map, which depends only on the version.
func decodeQR(t *testing.T, q *qrCode) []byte {
	t.Helper()
	n := q.size
	// Format information, first copy.
	var fmtBits int
	for i := 0; i <= 5; i++ {
		fmtBits |= b2i(q.modules[i][8]) << i
	}
	fmtBits |= b2i(q.modules[7][8])<<6 | b2i(q.modules[8][8])<<7 | b2i(q.modules[8][7])<<8
	for i := 9; i < 15; i++ {
		fmtBits |= b2i(q.modules[8][14-i]) << i
	}
	// Second copy must agree.
	var second int
	for i := 0; i < 8; i++ {
		second |= b2i(q.modules[8][n-1-i]) << i
	}
	for i := 8; i < 15; i++ {
		second |= b2i(q.modules[n-15+i][8]) << i
	}
	if fmtBits != second {
		t.Fatalf("format copies differ: %015b %015b", fmtBits, second)
	}
	fmtBits ^= 0x5412
	if fmtBits>>13 != 0 {
		t.Fatalf("error correction level bits %b, want M (00)", fmtBits>>13)
	}
	rem := fmtBits >> 10 << 10
	for i := 4; i >= 0; i-- {
		if rem>>(i+10)&1 == 1 {
			rem ^= 0x537 << i
		}
	}
	if rem != fmtBits&0x3ff {
		t.Fatal("format BCH check failed")
	}
	mask := fmtBits >> 10 & 7

	// Unmask a copy and read the codewords.
	c := newQR(q.version)
	c.drawFunctionPatterns()
	for y := range c.modules {
		copy(c.modules[y], q.modules[y])
	}
	c.applyMask(mask)
	var bits []bool
	for right := n - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < n; vert++ {
			for j := 0; j < 2; j++ {
				x, y := right-j, vert
				if (right+1)&2 == 0 {
					y = n - 1 - vert
				}
				if !c.fn[y][x] {
					bits = append(bits, c.modules[y][x])
				}
			}
		}
	}
	total := qrRawModules(q.version) / 8
	cw := make([]byte, total)
	for i := range cw {
		for k := 0; k < 8; k++ {
			if bits[i*8+k] {
				cw[i] |= 1 << (7 - k)
			}
		}
	}
	// De-interleave into blocks and check each one's ECC.
	nb, ecc := qrNumBlocks[q.version], qrEccPerBlock[q.version]
	nShort, shortLen := nb-total%nb, total/nb
	blocks := make([][]byte, nb)
	k := 0
	for i := 0; i < shortLen+1; i++ {
		for j := 0; j < nb; j++ {
			if i == shortLen-ecc && j < nShort {
				blocks[j] = append(blocks[j], 0) // placeholder
				continue
			}
			blocks[j] = append(blocks[j], cw[k])
			k++
		}
	}
	var data []byte
	for j, b := range blocks {
		dataLen := len(b) - ecc
		d := b[:dataLen]
		if j < nShort {
			d = d[:dataLen-1]
		}
		if want := rsRemainder(d, rsDivisor(ecc)); !bytes.Equal(want, b[dataLen:]) {
			t.Fatalf("block %d: Reed-Solomon check failed", j)
		}
		data = append(data, d...)
	}
	// Byte-mode payload.
	var br bitReader
	br.b = data
	if mode := br.read(4); mode != 0b0100 {
		t.Fatalf("mode %04b, want byte mode", mode)
	}
	cnt := 8
	if q.version >= 10 {
		cnt = 16
	}
	length := br.read(cnt)
	out := make([]byte, length)
	for i := range out {
		out[i] = byte(br.read(8))
	}
	return out
}

type bitReader struct {
	b   []byte
	pos int
}

func (r *bitReader) read(n int) int {
	v := 0
	for i := 0; i < n; i++ {
		v = v<<1 | int(r.b[r.pos/8]>>(7-r.pos%8)&1)
		r.pos++
	}
	return v
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestQRRoundTrip(t *testing.T) {
	url := "https://athena.example.ts.net:8443/enroll#" + strings.Repeat("Ab3-_", 9)[:43]
	for _, text := range []string{
		"a", "hello world", "https://a.b/enroll#x",
		url,                              // ~85 bytes: version 5 or 6
		strings.Repeat("0123456789", 10), // exactly 100
		strings.Repeat("é€", 40),         // multi-byte UTF-8
		strings.Repeat("x", 213),         // the largest that fits (version 10)
		strings.Repeat("y", 14),          // fills version 1
		strings.Repeat("z", 15),          // spills into version 2
		strings.Repeat("w", 106),         // fills version 6, with block structure
		strings.Repeat("v", 107),         // version 7: version information
	} {
		q, err := encodeQR([]byte(text))
		if err != nil {
			t.Fatalf("%.20q (%d bytes): %v", text, len(text), err)
		}
		if got := decodeQR(t, q); string(got) != text {
			t.Fatalf("version %d: decoded %q, want %q", q.version, got, text)
		}
	}
	if q, _ := encodeQR([]byte(url)); q.version > 6 {
		t.Fatalf("a %d-byte URL needs version %d, want at most 6", len(url), q.version)
	}
	if _, err := encodeQR(bytes.Repeat([]byte("x"), 214)); err == nil {
		t.Fatal("214 bytes accepted")
	}
}

func TestQRVersionInfoAndFinders(t *testing.T) {
	q, err := encodeQR(bytes.Repeat([]byte("v"), 107))
	if err != nil || q.version != 7 {
		t.Fatalf("version %d %v", q.version, err)
	}
	// Version 7 information is 0x07C94 in both places.
	var a, b int
	for i := 0; i < 18; i++ {
		a |= b2i(q.modules[i/3][q.size-11+i%3]) << i
		b |= b2i(q.modules[q.size-11+i%3][i/3]) << i
	}
	if a != 0x07C94 || b != 0x07C94 {
		t.Fatalf("version info %#x %#x, want 0x07c94", a, b)
	}
	// Finder patterns: dark ring, light ring, dark 3x3 core at each of three corners.
	for _, c := range [][2]int{{0, 0}, {q.size - 7, 0}, {0, q.size - 7}} {
		for dy := 0; dy < 7; dy++ {
			for dx := 0; dx < 7; dx++ {
				ring := max(abs(dx-3), abs(dy-3))
				if got, want := q.modules[c[1]+dy][c[0]+dx], ring != 2; got != want {
					t.Fatalf("finder at %v module (%d,%d) = %v", c, dx, dy, got)
				}
			}
		}
	}
}

func TestQRRender(t *testing.T) {
	q, _ := encodeQR([]byte("hello"))
	var plain, colored bytes.Buffer
	q.render(&plain, false)
	q.render(&colored, true)
	lines := strings.Split(strings.TrimRight(plain.String(), "\n"), "\n")
	if want := (q.size + 2*qrQuiet + 1) / 2; len(lines) != want {
		t.Fatalf("%d lines, want %d", len(lines), want)
	}
	for _, l := range lines {
		if n := len([]rune(l)); n != q.size+2*qrQuiet {
			t.Fatalf("line of %d columns, want %d", n, q.size+2*qrQuiet)
		}
	}
	if strings.Contains(plain.String(), "\x1b") || !strings.Contains(colored.String(), "\x1b[30;47m") {
		t.Fatal("color escapes in the wrong output")
	}
	if !strings.ContainsAny(plain.String(), "▀▄█") {
		t.Fatal("no half blocks drawn")
	}
}

// TestQRReedSolomonVector pins the error correction to the worked example in the QR tutorial literature ("HELLO
// WORLD", version 1-M), so the round trip above is not two copies of one mistake.
func TestQRReedSolomonVector(t *testing.T) {
	data := []byte{32, 91, 11, 120, 209, 114, 220, 77, 67, 64, 236, 17, 236, 17, 236, 17}
	want := []byte{196, 35, 39, 119, 235, 215, 231, 226, 93, 23}
	if got := rsRemainder(data, rsDivisor(10)); !bytes.Equal(got, want) {
		t.Fatalf("ECC = %v, want %v", got, want)
	}
}
