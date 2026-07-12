package mail

import (
	"testing"
	"time"
)

func TestGo(t *testing.T) {
	// Disabled config: returns without spawning a sender.
	Go(Config{}, "a@b.com", "s", "b")
	// Empty recipient: also a no-op.
	Go(Config{Host: "h", From: "f"}, "", "s", "b")
	// Enabled + recipient: spawns a goroutine that dials a dead relay and
	// logs the failure. Must not block or panic; give it a moment to run.
	Go(Config{Host: "127.0.0.1", Port: 1, From: "f@x"}, "to@x", "s", "b")
	time.Sleep(50 * time.Millisecond)
}

func TestDisabledIsNoop(t *testing.T) {
	// No host = disabled: Send returns nil without dialing.
	if err := Send(Config{}, "a@b.com", "s", "body"); err != nil {
		t.Fatalf("disabled Send should no-op, got %v", err)
	}
	// Enabled but empty recipient also no-ops.
	if err := Send(Config{Host: "smtp.example.com", From: "x@y.com"}, "", "s", "b"); err != nil {
		t.Fatalf("empty recipient should no-op, got %v", err)
	}
}

func TestFromAddr(t *testing.T) {
	cases := map[string]string{
		"Xilo <cache@example.com>": "cache@example.com",
		"cache@example.com":        "cache@example.com",
		"No Brackets Here":         "No Brackets Here",
	}
	for in, want := range cases {
		if got := fromAddr(in); got != want {
			t.Errorf("fromAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnabled(t *testing.T) {
	if (Config{}).Enabled() {
		t.Error("zero config should be disabled")
	}
	if (Config{Host: "h"}).Enabled() {
		t.Error("host without from should be disabled")
	}
	if !(Config{Host: "h", From: "f"}).Enabled() {
		t.Error("host+from should be enabled")
	}
}
