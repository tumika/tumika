package daemon

import (
	"testing"
	"time"
)

// The jitter spreads a fleet's update checks. Derived from the binary path
// rather than random, so the same host picks the same offset on every boot — a
// random one would re-roll on each restart, and a daemon that restarts often
// would check far more eagerly than its interval suggests.
func TestJitterIsStableAndBounded(t *testing.T) {
	const interval = 6 * time.Hour
	span := interval / 10

	first := jitterFor("/var/lib/tumika/bin/tumika", interval)
	for range 10 {
		if again := jitterFor("/var/lib/tumika/bin/tumika", interval); again != first {
			t.Fatalf("jitter is not stable: %s then %s", first, again)
		}
	}

	if first < 0 {
		t.Errorf("jitter = %s, which is negative — the timer would fire immediately "+
			"and the spread this exists to create would silently vanish", first)
	}
	if first >= span {
		t.Errorf("jitter = %s, want less than %s", first, span)
	}
}

// Different hosts must land on different offsets, or there is no spread at all.
func TestJitterDiffersByPath(t *testing.T) {
	const interval = 6 * time.Hour

	seen := map[time.Duration]bool{}
	for _, path := range []string{
		"/var/lib/tumika/bin/tumika",
		"/home/pi/.local/state/tumika/bin/tumika",
		"/opt/tumika/tumika",
		"/usr/local/bin/tumika",
	} {
		seen[jitterFor(path, interval)] = true
	}
	if len(seen) < 3 {
		t.Errorf("four paths produced %d distinct offsets; the spread is too weak", len(seen))
	}
}

// THE regression. Converting the hash to a Duration BEFORE the modulo produces
// a negative value whenever the high bit is set — half of all inputs — and a
// negative jitter fires the timer immediately. Many paths, so a one-in-two bug
// cannot hide.
func TestJitterIsNeverNegative(t *testing.T) {
	const interval = 6 * time.Hour

	for i := range 500 {
		path := "/var/lib/tumika/bin/tumika-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if got := jitterFor(path, interval); got < 0 {
			t.Fatalf("jitterFor(%q) = %s, which is negative", path, got)
		}
	}
}

// A tiny interval leaves no room to spread, and must not divide by zero.
func TestJitterWithNoRoomToSpread(t *testing.T) {
	for _, interval := range []time.Duration{0, time.Nanosecond, 5 * time.Nanosecond} {
		if got := jitterFor("/var/lib/tumika/bin/tumika", interval); got != 0 {
			t.Errorf("jitterFor(_, %s) = %s, want 0", interval, got)
		}
	}
}
