package main

import "testing"

func TestResolvedVersionPrefersStampedRelease(t *testing.T) {
	if got := resolvedVersion("v0.1.0"); got != "0.1.0" {
		t.Fatalf("resolved version = %q, want 0.1.0", got)
	}
	if got := resolvedVersion("0.1.0"); got != "0.1.0" {
		t.Fatalf("resolved version = %q, want 0.1.0", got)
	}
}
