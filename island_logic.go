package main

import (
	"math"
	"time"
)

// Island row geometry in 96-DPI logical pixels. These are shared by painting,
// hover hit-testing and click hit-testing so the three cannot drift apart.
const (
	islandActionStartX  = 165
	islandActionSplitX  = 250
	islandLeaveDuration = 180 * time.Millisecond
)

// islandHoverTarget returns the visible row and action half under the cursor.
// half is -1 for the timer-text part of a row, 0 for the first action and 1
// for the second. row is -1 outside every row.
func islandHoverTarget(x, y, top, rowHeight, rows, firstX, secondX int) (row, half int) {
	row = -1
	half = -1
	if rows <= 0 || rowHeight <= 0 || y < top {
		return
	}
	row = (y - top) / rowHeight
	if row < 0 || row >= rows {
		row = -1
		return
	}
	if x < firstX {
		return
	}
	if x < secondX {
		half = 0
	} else {
		half = 1
	}
	return
}

func islandLeaveFraction(start, now time.Time) (float64, bool) {
	if start.IsZero() {
		return 0, true
	}
	elapsed := now.Sub(start)
	if elapsed <= 0 {
		return 0, false
	}
	if elapsed >= islandLeaveDuration {
		return 1, true
	}
	return easeInCubic(float64(elapsed) / float64(islandLeaveDuration)), false
}

// islandAlphaMask returns one coverage byte per pixel for a rounded
// rectangle of width x height whose corner ellipse is corner wide (the
// GDI convention: CreateRoundRectRgn and RoundRect take the ellipse
// width/height, so the geometric radius is corner/2). Coverage is 255
// inside, 0 outside, with a ~1px linear falloff at the edge.
func islandAlphaMask(width, height, corner int) []byte {
	mask := make([]byte, width*height)
	if width <= 0 || height <= 0 {
		return mask
	}
	r := float64(corner) / 2
	if r <= 0 {
		for i := range mask {
			mask[i] = 255
		}
		return mask
	}
	if maxR := math.Min(float64(width), float64(height)) / 2; r > maxR {
		r = maxR
	}
	w, h := float64(width), float64(height)
	for y := 0; y < height; y++ {
		cy := float64(y) + 0.5
		for x := 0; x < width; x++ {
			mask[y*width+x] = islandPixelCoverage(float64(x)+0.5, cy, w, h, r)
		}
	}
	return mask
}

// islandPixelCoverage returns the coverage (0-255) of the pixel centred
// at (cx, cy) inside the rounded rectangle [0,w) x [0,h) with corner
// radius r.
func islandPixelCoverage(cx, cy, w, h, r float64) byte {
	// Inside the central cross: full coverage without a sqrt.
	if cx >= r && cx < w-r && cy >= 0 && cy < h {
		return 255
	}
	if cy >= r && cy < h-r && cx >= 0 && cx < w {
		return 255
	}
	// Corner zone: distance to the nearest corner circle centre.
	ccx := r
	if cx >= w-r {
		ccx = w - r
	}
	ccy := r
	if cy >= h-r {
		ccy = h - r
	}
	d := math.Hypot(cx-ccx, cy-ccy)
	switch {
	case d <= r-0.5:
		return 255
	case d >= r+0.5:
		return 0
	default:
		return byte(255 * (r + 0.5 - d))
	}
}

// islandPointInRoundedRect reports whether (x, y) lies inside the
// rounded rectangle with the same geometry as islandAlphaMask (corner
// is the GDI ellipse width/height). Used for WM_NCHITTEST so the
// transparent corners stay click-through.
func islandPointInRoundedRect(x, y, width, height, corner int) bool {
	if x < 0 || y < 0 || x >= width || y >= height {
		return false
	}
	r := float64(corner) / 2
	if r <= 0 {
		return true
	}
	if maxR := math.Min(float64(width), float64(height)) / 2; r > maxR {
		r = maxR
	}
	return islandPixelCoverage(float64(x)+0.5, float64(y)+0.5, float64(width), float64(height), r) >= 128
}

// premultiplyIslandPixels applies mask as the alpha channel and
// premultiplies the colour channels, in place. pixels holds 32-bit
// values as written by GDI into a BI_RGB DIB section: bytes in memory
// are B, G, R, A, so as a little-endian uint32 the layout is
// B | G<<8 | R<<16 | A<<24. GDI leaves the alpha byte zero, so this
// also supplies it; fully covered pixels keep their colours.
func premultiplyIslandPixels(pixels []uint32, mask []byte) {
	n := len(pixels)
	if len(mask) < n {
		n = len(mask)
	}
	for i := 0; i < n; i++ {
		a := mask[i]
		if a == 0 {
			pixels[i] = 0
			continue
		}
		v := pixels[i]
		if a == 255 {
			pixels[i] = v | 0xFF000000
			continue
		}
		b := v & 0xFF
		g := (v >> 8) & 0xFF
		r := (v >> 16) & 0xFF
		ai := uint32(a)
		pixels[i] = ai<<24 | (r*ai/255)<<16 | (g*ai/255)<<8 | b*ai/255
	}
}

// applyIslandMask writes mask into pixels as the alpha channel and
// premultiplies the colours, row by row. pixels holds a bitmap whose
// rows are stride wide; only the left width pixels of each row belong
// to the window. Treating the bitmap as if its rows were width wide
// shears the mask and leaves the lower rows with no alpha at all.
func applyIslandMask(pixels []uint32, stride, width, height int, mask []byte) {
	if stride < width || width <= 0 || height <= 0 {
		return
	}
	if len(pixels) < (height-1)*stride+width || len(mask) < width*height {
		return
	}
	for y := 0; y < height; y++ {
		row := pixels[y*stride : y*stride+width]
		premultiplyIslandPixels(row, mask[y*width:y*width+width])
	}
}
