package main

import "time"

// animation is the platform-independent part of the window chrome animation.
// The Win32 backend only turns its scalar progress into alpha and Y offset.
type animation struct {
	kind     int
	start    time.Time
	duration time.Duration
	from     float64
	to       float64
}

const (
	animNone = iota
	animEnter
	animExit
)

func easeOutCubic(t float64) float64 {
	t = clamp01(t)
	inv := 1 - t
	return 1 - inv*inv*inv
}

func easeInCubic(t float64) float64 {
	t = clamp01(t)
	return t * t * t
}

func clamp01(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func (a *animation) begin(kind int, now time.Time, duration time.Duration, from, to float64) {
	a.kind = kind
	a.start = now
	a.duration = duration
	a.from = from
	a.to = to
}

// progress returns the eased scalar at now, and whether the animation is done.
func (a *animation) progress(now time.Time) (float64, bool) {
	if a.kind == animNone {
		return a.to, true
	}
	if a.duration <= 0 {
		return a.to, true
	}
	elapsed := now.Sub(a.start)
	if elapsed >= a.duration {
		return a.to, true
	}
	raw := float64(elapsed) / float64(a.duration)
	if a.kind == animExit {
		return a.from + (a.to-a.from)*easeInCubic(raw), false
	}
	return a.from + (a.to-a.from)*easeOutCubic(raw), false
}

func (a *animation) stop() {
	a.kind = animNone
	a.duration = 0
}
