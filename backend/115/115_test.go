package drive115

import (
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
)

// resetWAF puts the process wide WAF state back to a known zero value so tests
// do not depend on each other.
func resetWAF() {
	wafMu.Lock()
	wafUntil = time.Time{}
	wafNext = 0
	wafURLs = map[string]time.Time{}
	wafMu.Unlock()
}

// The first block must use the configured base and each consecutive block must
// double it, up to the cap. A fixed cooldown is what made a three minute WAF
// blip cost a ten minute outage.
func TestWAFCooldownEscalatesThenCaps(t *testing.T) {
	resetWAF()
	base, max := time.Minute, 4*time.Minute

	want := []time.Duration{base, 2 * time.Minute, max, max}
	for i, w := range want {
		if got := markWAFBlocked(base, max); got != w {
			t.Fatalf("block %d: cooldown = %v, want %v", i, got, w)
		}
	}
}

// A successful API call must drop the escalation back to the base, so blocks
// spread over time do not leave the mount backing off for half an hour.
func TestWAFSuccessResetsEscalation(t *testing.T) {
	resetWAF()
	base, max := time.Minute, 30*time.Minute

	markWAFBlocked(base, max) // 1m, next 2m
	markWAFBlocked(base, max) // 2m, next 4m
	wafSuccess()
	if got := markWAFBlocked(base, max); got != base {
		t.Fatalf("cooldown after success = %v, want %v", got, base)
	}
}

// A zero or negative waf_cooldown must not disable the cooldown entirely.
func TestWAFCooldownFallsBackToDefault(t *testing.T) {
	resetWAF()
	if got := markWAFBlocked(0, 0); got != defaultWAFCooldown {
		t.Fatalf("cooldown with unset options = %v, want %v", got, defaultWAFCooldown)
	}
}

// Parking one listing endpoint must leave the others usable: that is what makes
// the multi endpoint fallback work on a WAF error instead of taking the whole
// mount down.
func TestURLBlockIsPerEndpoint(t *testing.T) {
	resetWAF()
	a, b := "https://a.example/files", "https://b.example/files"

	markURLBlocked(a, time.Minute, time.Minute)
	if !urlWAFBlocked(a) {
		t.Fatal("parked endpoint reported as usable")
	}
	if urlWAFBlocked(b) {
		t.Fatal("unrelated endpoint reported as parked")
	}
}

// An expired endpoint block must be reported as usable and be forgotten.
func TestURLBlockExpires(t *testing.T) {
	resetWAF()
	url := "https://a.example/files"

	wafMu.Lock()
	wafURLs[url] = time.Now().Add(-time.Second)
	wafMu.Unlock()

	if urlWAFBlocked(url) {
		t.Fatal("expired block still reported as parked")
	}
	wafMu.Lock()
	_, still := wafURLs[url]
	wafMu.Unlock()
	if still {
		t.Fatal("expired block was not forgotten")
	}
}

// orderedURLs must prefer the last known good endpoint, keep the configured
// order for the rest, and skip endpoints that are parked. An empty result is
// what makes listDir fall back to the process wide cooldown.
func TestOrderedURLsSkipsParkedEndpoints(t *testing.T) {
	resetWAF()
	fsys := &Fs{opt: Options{ListAPIURLs: fs.CommaSepList{"https://a", "https://b", "https://c"}}}

	gcache.mu.Lock()
	prev := gcache.listURL
	gcache.listURL = "https://b"
	gcache.mu.Unlock()
	defer func() {
		gcache.mu.Lock()
		gcache.listURL = prev
		gcache.mu.Unlock()
	}()

	markURLBlocked("https://b", time.Minute, time.Minute)
	got := fsys.orderedURLs()
	want := []string{"https://a", "https://c"}
	if len(got) != len(want) {
		t.Fatalf("orderedURLs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("orderedURLs = %v, want %v", got, want)
		}
	}

	markURLBlocked("https://a", time.Minute, time.Minute)
	markURLBlocked("https://c", time.Minute, time.Minute)
	if got := fsys.orderedURLs(); len(got) != 0 {
		t.Fatalf("orderedURLs with every endpoint parked = %v, want empty", got)
	}
}
