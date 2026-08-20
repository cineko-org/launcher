package managedfiles

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteJSONAtomicReplacesExistingManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installed.json")
	if err := WriteJSONAtomic(path, map[string]int{"generation": 1}); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSONAtomic(path, map[string]int{"generation": 2}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path) // #nosec G304 -- test-owned path.
	if err != nil || !bytes.Contains(contents, []byte(`"generation": 2`)) {
		t.Fatalf("replacement manifest=%q error=%v", contents, err)
	}
}
