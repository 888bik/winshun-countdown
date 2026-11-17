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
