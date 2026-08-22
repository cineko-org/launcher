package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	releasepb "github.com/cineko-org/contracts/v3/gen/go/cineko/release"
	runtimearchive "github.com/cineko-org/launcher/internal/launcher/archive"
	"github.com/cineko-org/launcher/internal/launcher/managedfiles"
)

const componentIntegrityFilename = ".cineko-integrity.json"

type componentIntegrity struct {
	ArtifactSHA256 string `json:"artifactSha256"`
	TreeSHA256     string `json:"treeSha256"`
}

// Component identifies one immutable runtime component and its executable.
type Component struct {
	Name     string
	Artifact *releasepb.Artifact
}

// LoadComponent validates an installed component and returns its launch path.
func LoadComponent(root string, item Component) (string, error) {
	if err := managedfiles.ValidateDirectory(filepath.Dir(filepath.Dir(root)), root); err != nil {
		return "", err
	}
	if err := validateComponentIntegrity(root, item.Artifact.GetSha256()); err != nil {
		return "", err
	}
	executable := filepath.Join(root, filepath.FromSlash(item.Artifact.GetExecutable()))
	if err := requireExecutable(executable); err != nil {
		return "", err
	}
	if item.Name == "playwright" {
		if err := requirePlaywrightDriver(root); err != nil {
			return "", err
		}
		return root, nil
	}
	return executable, nil
}

// ActivateComponent extracts, validates, and atomically activates a component.
func ActivateComponent(archivePath string, root string, item Component) (string, error) {
	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	if err := managedfiles.ValidateDirectory(filepath.Dir(filepath.Dir(parent)), parent); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(parent, ".install-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := runtimearchive.Extract(archivePath, item.Artifact.GetUrl(), staging); err != nil {
		return "", err
	}
	if err := writeComponentIntegrity(staging, item.Artifact.GetSha256()); err != nil {
		return "", fmt.Errorf("record %s integrity: %w", item.Name, err)
	}
	if _, err := LoadComponent(staging, item); err != nil {
		return "", fmt.Errorf("validate %s executable: %w", item.Name, err)
	}
	if err := managedfiles.RemoveManagedEntry(root); err != nil {
		return "", err
	}
	if err := os.Rename(staging, root); err != nil {
		return "", err
	}
	return LoadComponent(root, item)
}

func writeComponentIntegrity(root string, artifactSHA256 string) error {
	treeSHA256, err := componentTreeSHA256(root)
	if err != nil {
		return err
	}
	return managedfiles.WriteJSONAtomic(filepath.Join(root, componentIntegrityFilename), componentIntegrity{
		ArtifactSHA256: artifactSHA256,
		TreeSHA256:     treeSHA256,
	})
}

func validateComponentIntegrity(root string, artifactSHA256 string) error {
	contents, err := os.ReadFile(filepath.Join(root, componentIntegrityFilename)) // #nosec G304 -- digest-scoped component root.
	if err != nil {
		return errors.New("component integrity metadata is missing")
	}
	var metadata componentIntegrity
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		!strings.EqualFold(metadata.ArtifactSHA256, artifactSHA256) || len(metadata.TreeSHA256) != sha256.Size*2 {
		return errors.New("component integrity metadata is invalid")
	}
	actual, err := componentTreeSHA256(root)
	if err != nil || !strings.EqualFold(actual, metadata.TreeSHA256) {
		return errors.New("installed component integrity check failed")
	}
	return nil
}

func componentTreeSHA256(root string) (string, error) {
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer func() { _ = rootHandle.Close() }()

	hasher := sha256.New()
	entries := 0
	var expanded int64
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
				return errors.New("component root must be a directory")
			}
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == componentIntegrityFilename {
			return err
		}
		entries++
		if entries > runtimearchive.MaximumEntries {
			return errors.New("component tree contains too many entries")
		}
		name := filepath.ToSlash(relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(hasher, "%s\x00%o\x00%d\x00", name, info.Mode(), info.Size())
		return hashComponentEntry(rootHandle, relative, info, hasher, &expanded)
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func hashComponentEntry(root *os.Root, relative string, info os.FileInfo, writer io.Writer, expanded *int64) error {
	switch {
	case info.Mode().IsRegular():
		if info.Size() > runtimearchive.MaximumFileSize {
			return errors.New("component file exceeds size limit")
		}
		if info.Size() > runtimearchive.MaximumExpandedSize-*expanded {
			return errors.New("component tree exceeds expanded-size limit")
		}
		*expanded += info.Size()
		return hashComponentFile(root, relative, info, writer)
	case info.IsDir():
		return nil
	case info.Mode()&os.ModeSymlink != 0:
		target, err := root.Readlink(relative)
		if err != nil {
			return err
		}
		_, _ = io.WriteString(writer, target)
		return nil
	default:
		return errors.New("component tree contains a special file")
	}
}

func hashComponentFile(root *os.Root, relative string, expected os.FileInfo, writer io.Writer) error {
	file, err := root.Open(relative)
	if err != nil {
		return err
	}
	actual, statErr := file.Stat()
	if statErr != nil || !actual.Mode().IsRegular() || !os.SameFile(expected, actual) {
		_ = file.Close()
		return errors.Join(errors.New("component file changed while hashing"), statErr)
	}
	_, copyErr := io.Copy(writer, file)
	return errors.Join(copyErr, file.Close())
}

// Cleanup removes unreferenced runtime components and records failed removals.
func Cleanup(dataDir string, active map[string]string) {
	failed := make([]string, 0)
	for component, digest := range active {
		root := filepath.Join(dataDir, "components", component)
		cleanupDirectoryEntries(dataDir, root, digest, &failed)
	}
	cleanupDirectoryEntries(dataDir, filepath.Join(dataDir, "downloads"), "", &failed)
	runtimeRoot := filepath.Join(dataDir, "runtime")
	cleanupRuntimeDirectories(dataDir, runtimeRoot, &failed)
	pending := filepath.Join(runtimeRoot, "cleanup-pending.json")
	if len(failed) == 0 {
		_ = os.Remove(pending)
		return
	}
	_ = managedfiles.WriteJSONAtomic(pending, failed)
}

func cleanupDirectoryEntries(managedRoot string, root string, keep string, failed *[]string) {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() ||
		managedfiles.ValidateDirectory(managedRoot, root) != nil {
		*failed = append(*failed, root)
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		*failed = append(*failed, root)
		return
	}
	for _, entry := range entries {
		if entry.Name() == keep {
			continue
		}
		candidate := filepath.Join(root, entry.Name())
		if err := managedfiles.RemoveManagedEntry(candidate); err != nil {
			*failed = append(*failed, candidate)
		}
	}
}

func cleanupRuntimeDirectories(managedRoot string, root string, failed *[]string) {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() ||
		managedfiles.ValidateDirectory(managedRoot, root) != nil {
		*failed = append(*failed, root)
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		*failed = append(*failed, root)
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		candidate := filepath.Join(root, entry.Name())
		if err := managedfiles.RemoveManagedEntry(candidate); err != nil {
			*failed = append(*failed, candidate)
		}
	}
}

func requireExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("executable file is missing")
	}
	if runtimeExecutableBitRequired() && info.Mode().Perm()&0o111 == 0 {
		if err := os.Chmod(path, info.Mode().Perm()|0o700); err != nil {
			return fmt.Errorf("mark executable: %w", err)
		}
	}
	return nil
}

func requirePlaywrightDriver(root string) error {
	node := "node"
	if os.PathSeparator == '\\' {
		node = "node.exe"
	}
	for _, required := range []string{filepath.Join(root, node), filepath.Join(root, "package", "cli.js")} {
		if err := requireExecutable(required); err != nil {
			return fmt.Errorf("validate Playwright driver: %w", err)
		}
	}
	return nil
}

func runtimeExecutableBitRequired() bool { return os.PathSeparator == '/' }
