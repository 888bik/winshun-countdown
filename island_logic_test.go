package main

import (
	"testing"
	"time"
)

func TestIslandHoverTarget(t *testing.T) {
	// Rows start at 58, each 52 high. Action zones start at 165 and 250.
	cases := []struct {
		name      string
		x, y      int
		row, half int
	}{
		{"title", 100, 20, -1, -1},
		{"row text", 100, 70, 0, -1},
		{"row first action", 170, 70, 0, 0},
		{"row second action", 260, 70, 0, 1},
		{"second row", 260, 130, 1, 1},
		{"below rows", 260, 300, -1, -1},
	}
	for _, tc := range cases {
		row, half := islandHoverTarget(tc.x, tc.y, 58, 52, 3, islandActionStartX, islandActionSplitX)
		if row != tc.row || half != tc.half {
			t.Fatalf("%s: got row=%d half=%d, want row=%d half=%d", tc.name, row, half, tc.row, tc.half)
		}
	}
}

func TestIslandLeaveFraction(t *testing.T) {
	start := time.Unix(100, 0)
	if got, done := islandLeaveFraction(start, start); got != 0 || done {
		t.Fatalf("start = %v, %v", got, done)
	}
	if got, done := islandLeaveFraction(start, start.Add(90*time.Millisecond)); got <= 0 || got >= 1 || done {
		t.Fatalf("mid = %v, %v", got, done)
	}
	if got, done := islandLeaveFraction(start, start.Add(islandLeaveDuration)); got != 1 || !done {
		t.Fatalf("end = %v, %v", got, done)
	}
	if got, done := islandLeaveFraction(time.Time{}, start); got != 0 || !done {
		t.Fatalf("zero start = %v, %v", got, done)
	}
}

func TestIslandAlphaMask(t *testing.T) {
	mask := islandAlphaMask(20, 12, 8) // corner ellipse 8 -> radius 4
	if len(mask) != 20*12 {
		t.Fatalf("mask length = %d, want %d", len(mask), 20*12)
	}
	if got := mask[6*20+10]; got != 255 {
		t.Fatalf("centre = %d, want 255", got)
	}
	if got := mask[0]; got != 0 {
		t.Fatalf("far corner (0,0) = %d, want 0", got)
	}
	if got := mask[0*20+10]; got != 255 {
		t.Fatalf("top edge middle = %d, want 255", got)
	}
	if got := mask[6*20+0]; got != 255 {
		t.Fatalf("left edge middle = %d, want 255", got)
	}
	if got := mask[1*20+1]; got <= 0 || got >= 255 {
		t.Fatalf("near-corner (1,1) = %d, want partial coverage", got)
	}
	plain := islandAlphaMask(10, 10, 0)
	for i, v := range plain {
		if v != 255 {
			t.Fatalf("corner 0: mask[%d] = %d, want 255", i, v)
		}
	}
}

func TestIslandPointInRoundedRect(t *testing.T) {
	// 20x12, corner ellipse 8 -> radius 4, corner circle centre (4,4).
	cases := []struct {
		x, y int
		want bool
	}{
		{10, 6, true},  // centre
		{10, 0, true},  // top edge
		{0, 6, true},   // left edge
		{3, 3, true},   // inside the corner circle
		{1, 1, true},   // inside the ellipse (dist 3.54 < 4)
		{0, 1, false},  // outside the ellipse (dist 4.30 > 4)
		{0, 0, false},  // far corner
		{-1, 6, false}, // outside the window
		{20, 6, false}, // outside the window
	}
	for _, tc := range cases {
		if got := islandPointInRoundedRect(tc.x, tc.y, 20, 12, 8); got != tc.want {
			t.Fatalf("(%d,%d) = %v, want %v", tc.x, tc.y, got, tc.want)
		}
	}
}

func TestPremultiplyIslandPixels(t *testing.T) {
	// Pixels as GDI leaves them: RGB set, alpha byte zero.
	red := uint32(0x00FF0000) // R=255
	white := uint32(0x00FFFFFF)
	pixels := []uint32{red, red, white, 0}
	mask := []byte{255, 0, 128, 255}
	premultiplyIslandPixels(pixels, mask)
	if pixels[0] != 0xFFFF0000 {
		t.Fatalf("opaque red = %#x, want 0xFFFF0000", pixels[0])
	}
	if pixels[1] != 0 {
		t.Fatalf("masked-out = %#x, want 0", pixels[1])
	}
	if pixels[2] != 0x80808080 {
		t.Fatalf("half white = %#x, want 0x80808080", pixels[2])
	}
	if pixels[3] != 0xFF000000 {
		t.Fatalf("opaque black = %#x, want 0xFF000000", pixels[3])
	}
	// A mask shorter than the pixels leaves the tail untouched.
	tail := []uint32{red, red}
	premultiplyIslandPixels(tail, []byte{255})
	if tail[1] != red {
		t.Fatalf("tail = %#x, want untouched %#x", tail[1], red)
	}
}

func TestApplyIslandMaskStride(t *testing.T) {
	// Bitmap stride 10, window 6x4 (the 1.8.2 bug: the DIB is
	// allocated at the largest island size, so its stride is wider
	// than the window; applying the mask as if the rows were window
	// wide shears it and leaves the lower rows transparent).
	stride, width, height := 10, 6, 4
	pixels := make([]uint32, stride*height)
	for i := range pixels {
		pixels[i] = 0x00FFFFFF // white, alpha 0 (as GDI leaves it)
	}
	mask := make([]byte, width*height)
	for i := range mask {
		mask[i] = 255
	}
	mask[0] = 0 // top-left corner pixel transparent
	applyIslandMask(pixels, stride, width, height, mask)
	if pixels[0] != 0 {
		t.Fatalf("corner = %#x, want 0", pixels[0])
	}
	if pixels[5] != 0xFFFFFFFF {
		t.Fatalf("row 0 last = %#x, want 0xFFFFFFFF", pixels[5])
	}
	if pixels[6] != 0x00FFFFFF {
		t.Fatalf("row 0 padding = %#x, want untouched 0x00FFFFFF", pixels[6])
	}
	if pixels[10] != 0xFFFFFFFF {
		t.Fatalf("row 1 first = %#x, want 0xFFFFFFFF", pixels[10])
	}
	if pixels[35] != 0xFFFFFFFF {
		t.Fatalf("last row last = %#x, want 0xFFFFFFFF", pixels[35])
	}
	if pixels[39] != 0x00FFFFFF {
		t.Fatalf("last padding = %#x, want untouched 0x00FFFFFF", pixels[39])
	}
}

func TestIslandCollapsedWidthFor(t *testing.T) {
	pad, gap := 12, 8
	cases := []struct {
		name                  string
		titleW, textW, extraW int
		want                  int
	}{
		// Running timer: short middle text, the min-width clamp covers
		// the centred span in practice; the helper itself returns the
		// centred requirement (159).
		{"running", 32, 55, 0, 159},
		// Paused timer: long middle text ("04:46 已暂停"). Side by
		// side would be 164, but the centred span needs 204, or the
		// text starts inside the title.
		{"paused", 32, 100, 0, 204},
		// With a +N count the right side also binds.
		{"with extra", 32, 55, 15, 159},
		{"no title", 0, 55, 0, 95},
	}
	for _, tc := range cases {
		got := islandCollapsedWidthFor(tc.titleW, tc.textW, tc.extraW, pad, gap)
		if got != tc.want {
			t.Fatalf("%s: width = %d, want %d", tc.name, got, tc.want)
		}
		// The centred middle span must clear the title (with a gap)
		// on the left and the +N count (with a gap) on the right.
		left := (got - tc.textW) / 2
		right := (got + tc.textW) / 2
		if left < pad+tc.titleW+gap {
			t.Fatalf("%s: centred text starts at %d, inside title (want >= %d)", tc.name, left, pad+tc.titleW+gap)
		}
		if right > got-pad-tc.extraW-gap {
			t.Fatalf("%s: centred text ends at %d, inside +N (want <= %d)", tc.name, right, got-pad-tc.extraW-gap)
		}
	}
}
