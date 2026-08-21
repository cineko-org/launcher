package managedfiles

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidateDirectory verifies that candidate is a real directory below root.
func ValidateDirectory(root string, candidate string) error {
	root = filepath.Clean(root)
	candidate = filepath.Clean(candidate)
	if candidate != root && !pathWithin(root, candidate) {
		return errors.New("managed directory escapes its root")
	}
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return err
	}
	current := root
	parts := []string{}
	if relative != "." {
		parts = strings.Split(relative, string(filepath.Separator))
	}
	for index := -1; index < len(parts); index++ {
		if index >= 0 {
			current = filepath.Join(current, parts[index])
		}
		if err := validateRealDirectory(current); err != nil {
			return fmt.Errorf("managed path %q is not a real directory: %w", current, err)
		}
	}
	return nil
}

func validateRealDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("path is not a directory")
	}
	reparse, err := pathHasReparsePoint(path)
	if err != nil {
		return err
	}
	if reparse {
		return errors.New("path is a reparse point")
	}
	return nil
}

// RemoveManagedEntry removes one managed file, symlink, or directory safely.
func RemoveManagedEntry(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	reparse, err := pathHasReparsePoint(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || reparse || !info.IsDir() {
		return os.Remove(path)
	}
	return os.RemoveAll(path)
}

func pathWithin(root string, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// WriteJSONAtomic writes a JSON value and atomically replaces its destination.
func WriteJSONAtomic(path string, value any) error {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	return WriteAtomic(path, contents)
}

// WriteAtomic writes contents and atomically replaces its destination.
func WriteAtomic(path string, contents []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s directory: %w", filepath.Base(path), err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	temporary := file.Name()
	defer func() { _ = os.Remove(temporary) }()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := ReplaceAtomic(temporary, path); err != nil {
		return fmt.Errorf("activate %s: %w", filepath.Base(path), err)
	}
	return nil
}
