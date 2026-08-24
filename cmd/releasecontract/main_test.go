package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	releasepb "github.com/cineko-org/contracts/v3/gen/go/cineko/release"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestGeneratedLauncherReleaseSet(t *testing.T) {
	paths, payload := testReleaseSet(t)
	if len(paths) != 3 {
		t.Fatalf("release paths = %d", len(paths))
	}
	if !regexp.MustCompile(`"size":\s*"[1-9][0-9]*"`).Match(payload) {
		t.Fatalf("ProtoJSON int64 size was not encoded as a string: %s", payload)
	}

	set := &releasepb.LauncherReleaseSet{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(payload, set); err != nil {
		t.Fatal(err)
	}
	if len(set.GetReleases()) != 3 || releaseKey(set.GetReleases()[0]) != "darwin/arm64" || releaseKey(set.GetReleases()[2]) != "windows/amd64" {
		t.Fatalf("generated release set = %+v", set.GetReleases())
	}
	if _, err := readReleaseSet(paths[:2]); err == nil {
		t.Fatal("incomplete Launcher release set accepted")
	}
	if err := writeRelease(io.Discard, []string{
		"latest", "darwin/arm64", strings.TrimSuffix(paths[0], ".json"),
		"Cineko Launcher.app/Contents/MacOS/Cineko Launcher",
		"https://github.example/releases/darwin-arm64.zip", "2026-08-12T00:00:00Z",
	}); err == nil {
		t.Fatal("non-semantic Launcher version accepted")
	}
}

func TestReleaseContractHasNoRemotePublishCommand(t *testing.T) {
	if err := run([]string{"publish", "https://example.invalid", "release.json"}); err == nil {
		t.Fatal("retired remote publish command was accepted")
	}
}

func testReleaseSet(t *testing.T) ([]string, []byte) {
	t.Helper()
	root := t.TempDir()
	paths := make([]string, 0, 3)
	for _, target := range []struct {
		platform   string
		extension  string
		executable string
	}{
		{"darwin/arm64", "zip", "Cineko Launcher.app/Contents/MacOS/Cineko Launcher"},
		{"linux/amd64", "AppImage", "cineko-launcher-v1.2.3-linux-amd64.AppImage"},
		{"windows/amd64", "exe", "Cineko Launcher.exe"},
	} {
		artifact := filepath.Join(root, strings.ReplaceAll(target.platform, "/", "-")+"."+target.extension)
		if err := os.WriteFile(artifact, []byte("portable Launcher\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var release bytes.Buffer
		if err := writeRelease(&release, []string{
			"1.2.3", target.platform, artifact, target.executable,
			"https://github.example/releases/" + filepath.Base(artifact), "2026-08-12T00:00:00Z",
		}); err != nil {
			t.Fatal(err)
		}
		path := artifact + ".json"
		if err := os.WriteFile(path, release.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	var set bytes.Buffer
	if err := writeSet(&set, paths); err != nil {
		t.Fatal(err)
	}
	return paths, set.Bytes()
}
