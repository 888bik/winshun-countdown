package main

import "time"

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
