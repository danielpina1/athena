// A small QR Code encoder for `athena web enroll`: byte mode, error correction level M, versions 1 to 10 (up to 213
// bytes), drawn with Unicode half-blocks.
//
// This file is a Go translation, reduced to that scope, of Project Nayuki's QR Code generator library
// (https://www.nayuki.io/page/qr-code-generator-library), which is MIT-licensed:
//
//	Copyright (c) Project Nayuki. (MIT License)
//
//	Permission is hereby granted, free of charge, to any person obtaining a copy of
//	this software and associated documentation files (the "Software"), to deal in
//	the Software without restriction, including without limitation the rights to
//	use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
//	the Software, and to permit persons to whom the Software is furnished to do so,
//	subject to the following conditions:
//
//	The above copyright notice and this permission notice shall be included in all
//	copies or substantial portions of the Software.
//
//	THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
//	IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS
//	FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR
//	COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER
//	IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
//	CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
//
// The renderer and qr_test.go are Athena's own; the test decodes what the encoder produces from the matrix alone.
package cli

import (
	"errors"
	"io"
	"strings"
)

const (
	qrMaxVersion = 10
	// qrQuiet is the quiet zone around the symbol, in modules (the standard asks for 4).
	qrQuiet = 4
)

// Level M, per version 1 to 10: error correction codewords per block, and number of blocks.
var (
	qrEccPerBlock = [qrMaxVersion + 1]int{0, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26}
	qrNumBlocks   = [qrMaxVersion + 1]int{0, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5}
)

// qrCode is a finished symbol: modules[y][x] is true for a dark module.
type qrCode struct {
	version int
	size    int
	modules [][]bool
	fn      [][]bool // function modules (patterns, timing, format and version bits), which data never touches
}

// qrRawModules is the number of modules available to data and error correction in a version.
func qrRawModules(ver int) int {
	n := (16*ver+128)*ver + 64
	if ver >= 2 {
		a := ver/7 + 2
		n -= (25*a-10)*a - 55
		if ver >= 7 {
			n -= 36
		}
	}
	return n
}

func qrDataCodewords(ver int) int {
	return qrRawModules(ver)/8 - qrEccPerBlock[ver]*qrNumBlocks[ver]
}

// encodeQR encodes data in byte mode at error correction level M, in the smallest version that holds it.
func encodeQR(data []byte) (*qrCode, error) {
	ver := 0
	for v := 1; v <= qrMaxVersion; v++ {
		countBits := 8
		if v >= 10 {
			countBits = 16
		}
		if 4+countBits+8*len(data) <= 8*qrDataCodewords(v) {
			ver = v
			break
		}
	}
	if ver == 0 {
		return nil, errors.New("qr: text too long for a terminal code")
	}
	// Mode 0100, character count, data, terminator, padding to a codeword, then the alternating pad codewords.
	var bits []bool
	put := func(v, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, v>>i&1 == 1)
		}
	}
	put(0b0100, 4)
	if ver >= 10 {
		put(len(data), 16)
	} else {
		put(len(data), 8)
	}
	for _, b := range data {
		put(int(b), 8)
	}
	capBits := 8 * qrDataCodewords(ver)
	put(0, min(4, capBits-len(bits)))
	put(0, (8-len(bits)%8)%8)
	for pad := 0xEC; len(bits) < capBits; pad ^= 0xEC ^ 0x11 {
		put(pad, 8)
	}
	cw := make([]byte, len(bits)/8)
	for i, b := range bits {
		if b {
			cw[i/8] |= 1 << (7 - i%8)
		}
	}

	q := newQR(ver)
	q.drawFunctionPatterns()
	q.drawCodewords(qrInterleave(cw, ver))
	best, bestPenalty := 0, -1
	for m := 0; m < 8; m++ {
		q.applyMask(m)
		q.drawFormat(m)
		if p := q.penalty(); bestPenalty < 0 || p < bestPenalty {
			best, bestPenalty = m, p
		}
		q.applyMask(m) // XOR again: undo
	}
	q.applyMask(best)
	q.drawFormat(best)
	return q, nil
}

// qrInterleave splits the data codewords into blocks, appends each block's Reed-Solomon remainder, and interleaves.
func qrInterleave(data []byte, ver int) []byte {
	nb, ecc := qrNumBlocks[ver], qrEccPerBlock[ver]
	total := qrRawModules(ver) / 8
	nShort := nb - total%nb
	shortLen := total / nb
	div := rsDivisor(ecc)
	blocks := make([][]byte, nb)
	k := 0
	for i := range blocks {
		n := shortLen - ecc
		if i >= nShort {
			n++
		}
		d := data[k : k+n]
		k += n
		b := append([]byte{}, d...)
		if i < nShort {
			b = append(b, 0) // a placeholder where the long blocks' extra data codeword sits, so the ECC columns line up
		}
		blocks[i] = append(b, rsRemainder(d, div)...)
	}
	var out []byte
	for i := 0; i < len(blocks[nb-1]); i++ {
		for j, b := range blocks {
			// Short blocks have no codeword at the position the long ones' extra data codeword occupies.
			if i == shortLen-ecc && j < nShort {
				continue
			}
			out = append(out, b[i])
		}
	}
	return out
}

// GF(256) with the QR polynomial x^8+x^4+x^3+x^2+1 (0x11D).
func gfMul(x, y byte) byte {
	var z byte
	for i := 7; i >= 0; i-- {
		hi := z >> 7
		z = z<<1 ^ hi*0x1D
		z ^= (y >> i & 1) * x
	}
	return z
}

func rsDivisor(degree int) []byte {
	res := make([]byte, degree)
	res[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := range res {
			res[j] = gfMul(res[j], root)
			if j+1 < degree {
				res[j] ^= res[j+1]
			}
		}
		root = gfMul(root, 2)
	}
	return res
}

func rsRemainder(data, div []byte) []byte {
	res := make([]byte, len(div))
	for _, b := range data {
		f := b ^ res[0]
		copy(res, res[1:])
		res[len(res)-1] = 0
		for i := range res {
			res[i] ^= gfMul(div[i], f)
		}
	}
	return res
}

func newQR(ver int) *qrCode {
	size := ver*4 + 17
	q := &qrCode{version: ver, size: size, modules: make([][]bool, size), fn: make([][]bool, size)}
	for i := range q.modules {
		q.modules[i] = make([]bool, size)
		q.fn[i] = make([]bool, size)
	}
	return q
}

func (q *qrCode) setFn(x, y int, dark bool) {
	q.modules[y][x] = dark
	q.fn[y][x] = true
}

func (q *qrCode) alignPositions() []int {
	if q.version == 1 {
		return nil
	}
	n := q.version/7 + 2
	step := (q.version*4 + n*2 + 1) / (n*2 - 2) * 2
	pos := make([]int, n)
	pos[0] = 6
	for i, p := n-1, q.size-7; i >= 1; i, p = i-1, p-step {
		pos[i] = p
	}
	return pos
}

func (q *qrCode) drawFunctionPatterns() {
	for i := 0; i < q.size; i++ {
		q.setFn(6, i, i%2 == 0)
		q.setFn(i, 6, i%2 == 0)
	}
	finder := func(cx, cy int) {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				x, y := cx+dx, cy+dy
				if x < 0 || y < 0 || x >= q.size || y >= q.size {
					continue
				}
				d := max(abs(dx), abs(dy))
				q.setFn(x, y, d != 2 && d != 4)
			}
		}
	}
	finder(3, 3)
	finder(q.size-4, 3)
	finder(3, q.size-4)
	pos := q.alignPositions()
	for i, cx := range pos {
		for j, cy := range pos {
			if (i == 0 && j == 0) || (i == 0 && j == len(pos)-1) || (i == len(pos)-1 && j == 0) {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					q.setFn(cx+dx, cy+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	q.drawFormat(0) // reserve the format modules
	if q.version >= 7 {
		rem := q.version
		for i := 0; i < 12; i++ {
			rem = rem<<1 ^ (rem>>11)*0x1F25
		}
		bits := q.version<<12 | rem
		for i := 0; i < 18; i++ {
			dark := bits>>i&1 == 1
			a, b := q.size-11+i%3, i/3
			q.setFn(a, b, dark)
			q.setFn(b, a, dark)
		}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// drawFormat writes the 15 format bits (level M is 00, then the mask) in both places.
func (q *qrCode) drawFormat(mask int) {
	data := 0<<3 | mask
	rem := data
	for i := 0; i < 10; i++ {
		rem = rem<<1 ^ (rem>>9)*0x537
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return bits>>i&1 == 1 }
	for i := 0; i <= 5; i++ {
		q.setFn(8, i, bit(i))
	}
	q.setFn(8, 7, bit(6))
	q.setFn(8, 8, bit(7))
	q.setFn(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		q.setFn(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		q.setFn(q.size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		q.setFn(8, q.size-15+i, bit(i))
	}
	q.setFn(8, q.size-8, true) // the always-dark module
}

// drawCodewords places the bits in the two-column zigzag, from the bottom right, skipping function modules.
func (q *qrCode) drawCodewords(data []byte) {
	i := 0
	for right := q.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < q.size; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = q.size - 1 - vert
				}
				if !q.fn[y][x] && i < len(data)*8 {
					q.modules[y][x] = data[i>>3]>>(7-i&7)&1 == 1
					i++
				}
			}
		}
	}
}

func (q *qrCode) applyMask(m int) {
	for y := 0; y < q.size; y++ {
		for x := 0; x < q.size; x++ {
			var flip bool
			switch m {
			case 0:
				flip = (x+y)%2 == 0
			case 1:
				flip = y%2 == 0
			case 2:
				flip = x%3 == 0
			case 3:
				flip = (x+y)%3 == 0
			case 4:
				flip = (x/3+y/2)%2 == 0
			case 5:
				flip = x*y%2+x*y%3 == 0
			case 6:
				flip = (x*y%2+x*y%3)%2 == 0
			default:
				flip = ((x+y)%2+x*y%3)%2 == 0
			}
			if flip && !q.fn[y][x] {
				q.modules[y][x] = !q.modules[y][x]
			}
		}
	}
}

// penalty scores a masked symbol by the standard's four rules; the mask with the lowest score reads best.
func (q *qrCode) penalty() int {
	n := q.size
	score := 0
	line := func(at func(i int) bool) {
		run := 1
		for i := 1; i < n; i++ {
			if at(i) == at(i-1) {
				run++
				continue
			}
			if run >= 5 {
				score += 3 + run - 5
			}
			run = 1
		}
		if run >= 5 {
			score += 3 + run - 5
		}
		// 1:1:3:1:1 finder-like patterns with four light modules on a side
		for i := 0; i+11 <= n; i++ {
			for _, pat := range [2]string{"10111010000", "00001011101"} {
				match := true
				for k := 0; k < 11; k++ {
					if at(i+k) != (pat[k] == '1') {
						match = false
						break
					}
				}
				if match {
					score += 40
				}
			}
		}
	}
	for a := 0; a < n; a++ {
		line(func(i int) bool { return q.modules[a][i] })
		line(func(i int) bool { return q.modules[i][a] })
	}
	dark := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if q.modules[y][x] {
				dark++
			}
			if x+1 < n && y+1 < n && q.modules[y][x] == q.modules[y][x+1] && q.modules[y][x] == q.modules[y+1][x] && q.modules[y][x] == q.modules[y+1][x+1] {
				score += 3
			}
		}
	}
	total := n * n
	k := (abs(dark*20-total*10)+total-1)/total - 1
	return score + max(k, 0)*10
}

// render draws the symbol with its quiet zone, two module rows to a text line, using the half blocks ▀ ▄ █. With color
// the lines are black on white whatever the terminal's theme, which scanners need; without it (a pipe, a file) dark
// modules are the blocks and light ones are blank.
func (q *qrCode) render(w io.Writer, color bool) {
	dark := func(x, y int) bool {
		x, y = x-qrQuiet, y-qrQuiet
		return x >= 0 && y >= 0 && x < q.size && y < q.size && q.modules[y][x]
	}
	total := q.size + 2*qrQuiet
	var sb strings.Builder
	for y := 0; y < total; y += 2 {
		if color {
			sb.WriteString("\x1b[30;47m")
		}
		for x := 0; x < total; x++ {
			top, bottom := dark(x, y), dark(x, y+1)
			switch {
			case top && bottom:
				sb.WriteString("█")
			case top:
				sb.WriteString("▀")
			case bottom:
				sb.WriteString("▄")
			default:
				sb.WriteString(" ")
			}
		}
		if color {
			sb.WriteString("\x1b[0m")
		}
		sb.WriteString("\n")
	}
	io.WriteString(w, sb.String())
}
