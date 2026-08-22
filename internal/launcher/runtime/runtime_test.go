package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	releasepb "github.com/cineko-org/contracts/v3/gen/go/cineko/release"
)

func TestLoadComponentDetectsMutation(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "client")
	writeExecutableFixture(t, executable, []byte("original"))
	digest := strings.Repeat("a", 64)
	if err := writeComponentIntegrity(root, digest); err != nil {
		t.Fatal(err)
	}
	artifact := &releasepb.Artifact{}
	artifact.SetSha256(digest)
	artifact.SetExecutable("client")
	item := Component{Name: "client", Artifact: artifact}
	if _, err := LoadComponent(root, item); err != nil {
		t.Fatal(err)
	}
	writeExecutableFixture(t, executable, []byte("modified"))
	if _, err := LoadComponent(root, item); err == nil {
		t.Fatal("mutated installed component passed integrity validation")
	}
}

func TestCleanupRemovesOnlyUnreferencedComponentsAndArtifacts(t *testing.T) {
	dataDir := t.TempDir()
	active := map[string]string{
		"client":     strings.Repeat("a", 64),
		"browser":    strings.Repeat("b", 64),
		"playwright": strings.Repeat("c", 64),
	}
	for component, digest := range active {
		for _, directory := range []string{digest, "obsolete", ".install-abandoned"} {
			if err := os.MkdirAll(filepath.Join(dataDir, "components", component, directory), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, path := range []string{
		filepath.Join(dataDir, "downloads", "old.blob"),
		filepath.Join(dataDir, "downloads", "old.blob.part"),
		filepath.Join(dataDir, "runtime", "0.9.0", "installed.json"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	Cleanup(dataDir, active)
	for component, digest := range active {
		entries, err := os.ReadDir(filepath.Join(dataDir, "components", component))
		if err != nil || len(entries) != 1 || entries[0].Name() != digest {
			t.Fatalf("%s component entries=%v error=%v", component, entries, err)
		}
	}
	for _, root := range []string{filepath.Join(dataDir, "downloads"), filepath.Join(dataDir, "runtime", "0.9.0")} {
		entries, err := os.ReadDir(root)
		if err == nil && len(entries) != 0 || err != nil && !os.IsNotExist(err) {
			t.Fatalf("cleanup root=%s entries=%v error=%v", root, entries, err)
		}
	}
}

func TestCleanupDoesNotFollowManagedRootSymlink(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("symlink policy is host-dependent on Windows")
	}
	dataDir := t.TempDir()
	outside := t.TempDir()
	protected := filepath.Join(outside, "keep")
	if err := os.WriteFile(protected, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "components"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dataDir, "components", "browser")); err != nil {
		t.Fatal(err)
	}
	Cleanup(dataDir, map[string]string{
		"client":     strings.Repeat("a", 64),
		"browser":    strings.Repeat("b", 64),
		"playwright": strings.Repeat("c", 64),
	})
	if contents, err := os.ReadFile(protected); err != nil || string(contents) != "keep" { // #nosec G304 -- test-owned path.
		t.Fatalf("cleanup followed managed-root symlink: contents=%q error=%v", contents, err)
	}
}

func writeExecutableFixture(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil { // #nosec G302 -- executable test fixture.
		t.Fatal(err)
	}
}
