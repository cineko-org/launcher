package main

import "testing"

func TestResolvedCentralURLPrefersEnvironmentOverride(t *testing.T) {
	previous := launcherCentralURL
	launcherCentralURL = "https://embedded.example"
	t.Cleanup(func() { launcherCentralURL = previous })
	t.Setenv("CINEKO_CENTRAL_URL", " https://override.example ")
	if got := resolvedCentralURL(); got != "https://override.example" {
		t.Fatalf("resolved Central URL = %q", got)
	}
	t.Setenv("CINEKO_CENTRAL_URL", "")
	if got := resolvedCentralURL(); got != "https://embedded.example" {
		t.Fatalf("embedded Central URL = %q", got)
	}
}
