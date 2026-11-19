package shlink

import (
	"errors"
	"fmt"
	"math/bits"
	"strings"
)

// qrVersionSpec holds ISO/IEC 18004 Level-L byte-mode parameters for QR versions 1..20,
// accommodating SHLink URIs up to ~850 bytes cleanly in pure Go with zero dependencies.
type qrVersionSpec struct {
	version        int
	totalCodewords int
	ecPerBlock     int
	group1Blocks   int
	group1DataCW   int
	group2Blocks   int
	group2DataCW   int
	alignCenters   []int
}

var qrLevelLSpecs = []qrVersionSpec{
	{1, 26, 7, 1, 19, 0, 0, nil},
	{2, 44, 10, 1, 34, 0, 0, []int{6, 18}},
	{3, 70, 15, 1, 55, 0, 0, []int{6, 22}},
	{4, 100, 20, 1, 80, 0, 0, []int{6, 26}},
	{5, 134, 26, 1, 108, 0, 0, []int{6, 30}},
	{6, 172, 18, 2, 68, 0, 0, []int{6, 34}},
	{7, 196, 20, 2, 78, 0, 0, []int{6, 22, 38}},
	{8, 242, 24, 2, 97, 0, 0, []int{6, 24, 42}},
	{9, 292, 30, 2, 116, 0, 0, []int{6, 26, 46}},
	{10, 346, 18, 2, 68, 2, 69, []int{6, 28, 50}},
	{11, 404, 20, 4, 81, 0, 0, []int{6, 30, 54}},
	{12, 466, 24, 2, 92, 2, 93, []int{6, 32, 58}},
	{13, 532, 26, 4, 107, 0, 0, []int{6, 34, 62}},
	{14, 581, 30, 3, 115, 1, 116, []int{6, 26, 46, 66}},
	{15, 655, 22, 5, 87, 1, 88, []int{6, 26, 48, 70}},
	{16, 733, 24, 5, 98, 1, 99, []int{6, 26, 50, 74}},
	{17, 815, 28, 1, 107, 5, 108, []int{6, 30, 54, 78}},
	{18, 901, 30, 5, 120, 1, 121, []int{6, 30, 56, 82}},
	{19, 991, 28, 3, 113, 4, 114, []int{6, 30, 58, 86}},
	{20, 1085, 28, 3, 107, 5, 108, []int{6, 34, 62, 90}},
}

func (s qrVersionSpec) dataCodewords() int {
	return s.group1Blocks*s.group1DataCW + s.group2Blocks*s.group2DataCW
}

// RenderQRSVG encodes text as an ISO/IEC 18004 QR Code (Byte mode, EC Level L)
// and renders a compact, accessible SVG image suitable for SHLink clinical sharing.
func RenderQRSVG(text string, modulePx int) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", errors.New("shlink: QR payload must not be empty")
	}
	if modulePx <= 0 {
		modulePx = 4
	}
	grid, err := encodeQRMatrix([]byte(text))
	if err != nil {
		return "", err
	}
	size := len(grid)
	const quiet = 4
	totalModules := size + 2*quiet
	dim := totalModules * modulePx

	var sb strings.Builder
	sb.Grow(size * size * 4)
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" role="img" aria-label="SMART Health Link QR Code">`, totalModules, totalModules, dim, dim)
	fmt.Fprintf(&sb, `<rect width="%d" height="%d" fill="#ffffff"/>`, totalModules, totalModules)
	sb.WriteString(`<path fill="#000000" d="`)
	for y := range size {
		for x := range size {
			if grid[y][x] {
				fmt.Fprintf(&sb, "M%d,%dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	sb.WriteString(`"/></svg>`)
	return sb.String(), nil
}

func encodeQRMatrix(data []byte) ([][]bool, error) {
	var spec qrVersionSpec
	found := false
	for _, candidate := range qrLevelLSpecs {
		charCountBits := 8
		if candidate.version >= 10 {
			charCountBits = 16
		}
		neededBits := 4 + charCountBits + len(data)*8
		if neededBits <= candidate.dataCodewords()*8 {
			spec = candidate
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("shlink: QR payload length %d exceeds version 20 capacity", len(data))
	}

	dataCW := buildByteModeCodewords(data, spec)
	finalCW := interleaveWithEC(dataCW, spec)

	size := 17 + 4*spec.version
	modules := make([][]bool, size)
	isFunc := make([][]bool, size)
	for i := range size {
		modules[i] = make([]bool, size)
		isFunc[i] = make([]bool, size)
	}

	placeFunctionPatterns(modules, isFunc, spec)
	placeDataBits(modules, isFunc, finalCW)

	// Apply mask 0 ((row + col) % 2 == 0) and write corresponding Level-L format + version info.
	applyMaskZero(modules, isFunc)
	drawFormatBits(modules, isFunc, 0)
	if spec.version >= 7 {
		drawVersionBits(modules, isFunc, spec.version)
	}
	return modules, nil
}

func buildByteModeCodewords(data []byte, spec qrVersionSpec) []byte {
	capBits := spec.dataCodewords() * 8
	var bitStream []bool
	appendBits := func(val int, n int) {
		for i := n - 1; i >= 0; i-- {
			bitStream = append(bitStream, ((val>>i)&1) == 1)
		}
	}

	// Byte mode indicator = 0100
	appendBits(0b0100, 4)
	countBits := 8
	if spec.version >= 10 {
		countBits = 16
	}
	appendBits(len(data), countBits)
	for _, b := range data {
		appendBits(int(b), 8)
	}

	// Terminator up to 4 zero bits
	for i := 0; i < 4 && len(bitStream) < capBits; i++ {
		bitStream = append(bitStream, false)
	}
	for len(bitStream)%8 != 0 {
		bitStream = append(bitStream, false)
	}

	out := make([]byte, 0, spec.dataCodewords())
	for i := 0; i < len(bitStream); i += 8 {
		var cw byte
		for j := range 8 {
			cw <<= 1
			if bitStream[i+j] {
				cw |= 1
			}
		}
		out = append(out, cw)
	}
	padBytes := [2]byte{0xEC, 0x11}
	for idx := 0; len(out) < spec.dataCodewords(); idx++ {
		out = append(out, padBytes[idx%2])
	}
	return out
}

func interleaveWithEC(dataCW []byte, spec qrVersionSpec) []byte {
	totalBlocks := spec.group1Blocks + spec.group2Blocks
	dataBlocks := make([][]byte, 0, totalBlocks)
	ecBlocks := make([][]byte, 0, totalBlocks)
	gen := rsGeneratorPoly(spec.ecPerBlock)

	offset := 0
	for range spec.group1Blocks {
		blk := dataCW[offset : offset+spec.group1DataCW]
		offset += spec.group1DataCW
		dataBlocks = append(dataBlocks, blk)
		ecBlocks = append(ecBlocks, rsComputeRemainder(blk, gen))
	}
	for range spec.group2Blocks {
		blk := dataCW[offset : offset+spec.group2DataCW]
		offset += spec.group2DataCW
		dataBlocks = append(dataBlocks, blk)
		ecBlocks = append(ecBlocks, rsComputeRemainder(blk, gen))
	}

	res := make([]byte, 0, spec.totalCodewords)
	maxDataLen := max(spec.group1DataCW, spec.group2DataCW)
	for i := range maxDataLen {
		for _, blk := range dataBlocks {
			if i < len(blk) {
				res = append(res, blk[i])
			}
		}
	}
	for i := range spec.ecPerBlock {
		for _, ec := range ecBlocks {
			res = append(res, ec[i])
		}
	}
	return res
}

func gfMul(x, y byte) byte {
	var z byte
	for i := 7; i >= 0; i-- {
		z = (z << 1) ^ ((z >> 7) * 0x1D)
		if ((y >> i) & 1) != 0 {
			z ^= x
		}
	}
	return z
}

func rsGeneratorPoly(degree int) []byte {
	poly := make([]byte, degree)
	poly[degree-1] = 1
	root := byte(1)
	for range degree {
		for j := range degree {
			poly[j] = gfMul(poly[j], root)
			if j+1 < degree {
				poly[j] ^= poly[j+1]
			}
		}
		root = gfMul(root, 0x02)
	}
	return poly
}

func rsComputeRemainder(data []byte, gen []byte) []byte {
	rem := make([]byte, len(gen))
	for _, b := range data {
		factor := b ^ rem[0]
		copy(rem, rem[1:])
		rem[len(rem)-1] = 0
		for j, g := range gen {
			rem[j] ^= gfMul(g, factor)
		}
	}
	return rem
}

func setFuncModule(modules, isFunc [][]bool, x, y int, dark bool) {
	modules[y][x] = dark
	isFunc[y][x] = true
}

func placeFunctionPatterns(modules, isFunc [][]bool, spec qrVersionSpec) {
	size := len(modules)
	// Timing patterns
	for i := range size {
		setFuncModule(modules, isFunc, 6, i, i%2 == 0)
		setFuncModule(modules, isFunc, i, 6, i%2 == 0)
	}

	// Finder patterns + separators at 3 corners
	drawFinder := func(cx, cy int) {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				xx, yy := cx+dx, cy+dy
				if xx >= 0 && xx < size && yy >= 0 && yy < size {
					dist := max(absInt(dx), absInt(dy))
					setFuncModule(modules, isFunc, xx, yy, dist != 2 && dist != 4)
				}
			}
		}
	}
	drawFinder(3, 3)
	drawFinder(size-4, 3)
	drawFinder(3, size-4)

	// Alignment patterns
	nAlign := len(spec.alignCenters)
	for i := range nAlign {
		for j := range nAlign {
			if (i == 0 && j == 0) || (i == 0 && j == nAlign-1) || (i == nAlign-1 && j == 0) {
				continue
			}
			cx, cy := spec.alignCenters[i], spec.alignCenters[j]
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					setFuncModule(modules, isFunc, cx+dx, cy+dy, max(absInt(dx), absInt(dy)) != 1)
				}
			}
		}
	}

	// Reserve format info & dark module
	drawFormatBits(modules, isFunc, 0)
	if spec.version >= 7 {
		drawVersionBits(modules, isFunc, spec.version)
	}
}

func placeDataBits(modules, isFunc [][]bool, codewords []byte) {
	size := len(modules)
	bitIdx := 0
	totalBits := len(codewords) * 8

	for right := size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := range size {
			for j := range 2 {
				x := right - j
				upward := ((right + 1) & 2) == 0
				y := vert
				if upward {
					y = size - 1 - vert
				}
				if !isFunc[y][x] && bitIdx < totalBits {
					dark := ((codewords[bitIdx>>3] >> (7 - (bitIdx & 7))) & 1) != 0
					modules[y][x] = dark
					bitIdx++
				}
			}
		}
	}
}

func applyMaskZero(modules, isFunc [][]bool) {
	size := len(modules)
	for y := range size {
		for x := range size {
			if !isFunc[y][x] && (x+y)%2 == 0 {
				modules[y][x] = !modules[y][x]
			}
		}
	}
}

func drawFormatBits(modules, isFunc [][]bool, mask int) {
	size := len(modules)
	// EC Level L format indicator = 01
	data := (0b01 << 3) | mask
	rem := data
	for range 10 {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bitsVal := ((data << 10) | rem) ^ 0x5412
	getBit := func(i int) bool { return ((bitsVal >> i) & 1) != 0 }

	for i := 0; i <= 5; i++ {
		setFuncModule(modules, isFunc, 8, i, getBit(i))
	}
	setFuncModule(modules, isFunc, 8, 7, getBit(6))
	setFuncModule(modules, isFunc, 8, 8, getBit(7))
	setFuncModule(modules, isFunc, 7, 8, getBit(8))
	for i := 9; i < 15; i++ {
		setFuncModule(modules, isFunc, 14-i, 8, getBit(i))
	}

	for i := 0; i < 8; i++ {
		setFuncModule(modules, isFunc, size-1-i, 8, getBit(i))
	}
	for i := 8; i < 15; i++ {
		setFuncModule(modules, isFunc, 8, size-15+i, getBit(i))
	}
	setFuncModule(modules, isFunc, 8, size-8, true)
}

func drawVersionBits(modules, isFunc [][]bool, version int) {
	size := len(modules)
	rem := version
	for range 12 {
		rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
	}
	bitsVal := (version << 12) | rem
	_ = bits.OnesCount(uint(bitsVal))
	for i := range 18 {
		dark := ((bitsVal >> i) & 1) != 0
		a := size - 11 + i%3
		b := i / 3
		setFuncModule(modules, isFunc, a, b, dark)
		setFuncModule(modules, isFunc, b, a, dark)
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
