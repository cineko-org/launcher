package main

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestResolveLauncherDataDirUsesVisibleCinekoHome(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "Users", "cineko")
	got, err := resolveLauncherDataDir("", func() (string, error) { return home, nil })
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "cineko"); got != want {
		t.Fatalf("data dir = %q, want %q", got, want)
	}
}

func TestResolveLauncherDataDirRequiresAbsoluteOverride(t *testing.T) {
	if _, err := resolveLauncherDataDir("relative", func() (string, error) { return "", errors.New("unused") }); err == nil {
		t.Fatal("relative CINEKO_DATA_DIR was accepted")
	}
}
