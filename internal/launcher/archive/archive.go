package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
)

const (
	// MaximumEntries limits the number of files an archive may contain.
	MaximumEntries = 100_000
	// MaximumFileSize limits the size of one extracted file.
	MaximumFileSize = int64(4 << 30)
	// MaximumExpandedSize limits the total extracted archive size.
	MaximumExpandedSize          = int64(16 << 30)
	maximumArchiveExpansionRatio = int64(200)
	maximumArchiveExpansionSlack = int64(64 << 20)
)

// Extract validates and extracts a supported release archive into destination.
func Extract(archivePath string, sourceURL string, destination string) error {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	switch {
	case strings.HasSuffix(strings.ToLower(sourceURL), ".zip"):
		return extractZip(archivePath, destination)
	case strings.HasSuffix(strings.ToLower(sourceURL), ".tar.gz"), strings.HasSuffix(strings.ToLower(sourceURL), ".tgz"):
		return extractTarGz(archivePath, destination)
	default:
		return errors.New("artifact archive must be zip, tar.gz, or tgz")
	}
}

func extractZip(archivePath string, destination string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	entries, err := inspectZip(reader.File, destination)
	if err != nil {
		return err
	}
	for index, entry := range reader.File {
		target, err := archiveTarget(destination, entries[index])
		if err != nil {
			return err
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		source, err := entry.Open()
		if err != nil {
			return err
		}
		size, err := checkedArchiveSize(entry.UncompressedSize64)
		if err != nil {
			_ = source.Close()
			return err
		}
		if err := copyArchiveFile(target, source, entry.Mode(), size); err != nil {
			_ = source.Close()
			return err
		}
		if err := source.Close(); err != nil {
			return err
		}
	}
	return nil
}

func inspectZip(files []*zip.File, destination string) ([]string, error) {
	if len(files) > MaximumEntries {
		return nil, errors.New("archive contains too many entries")
	}
	entries := make([]string, len(files))
	types := make(map[string]bool, len(files))
	var expanded int64
	var compressed int64
	for index, entry := range files {
		name, err := inspectZipEntry(entry, destination, types, &expanded, &compressed)
		if err != nil {
			return nil, err
		}
		entries[index] = name
	}
	if err := validateArchiveTree(types); err != nil {
		return nil, err
	}
	if archiveExpansionRatioExceeded(expanded, compressed) {
		return nil, errors.New("archive expansion ratio exceeds limit")
	}
	return entries, nil
}

func inspectZipEntry(
	entry *zip.File,
	destination string,
	types map[string]bool,
	expanded *int64,
	compressed *int64,
) (string, error) {
	name, err := cleanArchivePath(entry.Name)
	if err != nil {
		return "", err
	}
	if _, err := archiveTarget(destination, name); err != nil {
		return "", err
	}
	if _, exists := types[name]; exists {
		return "", fmt.Errorf("archive contains duplicate entry %q", name)
	}
	if entry.Mode()&os.ModeSymlink != 0 || !entry.FileInfo().IsDir() && !entry.Mode().IsRegular() {
		return "", fmt.Errorf("archive entry %q has unsupported type", name)
	}
	size, err := checkedArchiveSize(entry.UncompressedSize64)
	if err != nil || size > MaximumFileSize {
		return "", fmt.Errorf("archive entry %q exceeds size limit", name)
	}
	if size > MaximumExpandedSize-*expanded {
		return "", errors.New("archive expanded size exceeds limit")
	}
	*expanded += size
	compressedSize, err := checkedArchiveSize(entry.CompressedSize64)
	if err != nil || compressedSize > MaximumExpandedSize {
		return "", errors.New("archive compressed size exceeds limit")
	}
	if compressedSize > MaximumExpandedSize-*compressed {
		*compressed = MaximumExpandedSize
	} else {
		*compressed += compressedSize
	}
	types[name] = entry.FileInfo().IsDir()
	return name, nil
}

func checkedArchiveSize(size uint64) (int64, error) {
	if size > math.MaxInt64 {
		return 0, errors.New("archive entry size exceeds supported range")
	}
	return int64(size), nil
}

func extractTarGz(archivePath string, destination string) error {
	index, err := inspectTarGz(archivePath, destination)
	if err != nil {
		return err
	}
	return walkTarGz(archivePath, func(header *tar.Header, reader *tar.Reader) error {
		name, err := cleanArchivePath(header.Name)
		if err != nil {
			return err
		}
		target, err := archiveTarget(destination, name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			return os.MkdirAll(target, 0o750)
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			mode := os.FileMode(0o600)
			if header.Mode&0o111 != 0 {
				mode = 0o700
			}
			return copyArchiveFile(target, reader, mode, header.Size)
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			return os.Symlink(filepath.FromSlash(index.symlinks[name]), target)
		default:
			return errors.New("archive contains an unsupported entry")
		}
	})
}

type tarArchiveIndex struct {
	entries  map[string]byte
	symlinks map[string]string
}

func inspectTarGz(archivePath string, destination string) (tarArchiveIndex, error) {
	archiveInfo, err := os.Stat(archivePath)
	if err != nil || !archiveInfo.Mode().IsRegular() {
		return tarArchiveIndex{}, errors.New("archive must be a regular file")
	}
	index := tarArchiveIndex{
		entries:  make(map[string]byte),
		symlinks: make(map[string]string),
	}
	var expanded int64
	err = walkTarGz(archivePath, func(header *tar.Header, _ *tar.Reader) error {
		return indexTarEntry(destination, index, header, &expanded)
	})
	if err != nil {
		return tarArchiveIndex{}, err
	}
	if err := validateTarArchiveIndex(index); err != nil {
		return tarArchiveIndex{}, err
	}
	if archiveExpansionRatioExceeded(expanded, archiveInfo.Size()) {
		return tarArchiveIndex{}, errors.New("archive expansion ratio exceeds limit")
	}
	return index, nil
}

func indexTarEntry(destination string, index tarArchiveIndex, header *tar.Header, expanded *int64) error {
	if len(index.entries) >= MaximumEntries {
		return errors.New("archive contains too many entries")
	}
	name, err := cleanArchivePath(header.Name)
	if err != nil {
		return err
	}
	if _, err := archiveTarget(destination, name); err != nil {
		return err
	}
	if _, exists := index.entries[name]; exists {
		return fmt.Errorf("archive contains duplicate entry %q", name)
	}
	switch header.Typeflag {
	case tar.TypeDir, tar.TypeReg:
		if header.Size < 0 || header.Size > MaximumFileSize {
			return fmt.Errorf("archive entry %q exceeds size limit", name)
		}
		if header.Size > MaximumExpandedSize-*expanded {
			return errors.New("archive expanded size exceeds limit")
		}
		*expanded += header.Size
		index.entries[name] = header.Typeflag
	case tar.TypeSymlink:
		link, err := cleanTarLink(header.Linkname)
		if err != nil {
			return fmt.Errorf("archive symlink %q: %w", name, err)
		}
		index.entries[name] = header.Typeflag
		index.symlinks[name] = link
	default:
		return fmt.Errorf("archive entry %q has unsupported type", name)
	}
	return nil
}

func validateTarArchiveIndex(index tarArchiveIndex) error {
	types := make(map[string]bool, len(index.entries))
	for name, entryType := range index.entries {
		types[name] = entryType == tar.TypeDir
	}
	if err := validateArchiveTree(types); err != nil {
		return err
	}
	for name := range index.entries {
		for ancestor := pathpkg.Dir(name); ancestor != "."; ancestor = pathpkg.Dir(ancestor) {
			if _, linked := index.symlinks[ancestor]; linked {
				return fmt.Errorf("archive entry %q traverses symlink %q", name, ancestor)
			}
		}
	}
	for name, link := range index.symlinks {
		target := pathpkg.Clean(pathpkg.Join(pathpkg.Dir(name), link))
		if !cleanArchiveRelativePath(target) {
			return fmt.Errorf("archive symlink %q escapes destination", name)
		}
		if _, err := resolveTarSymlinks(target, index.symlinks); err != nil {
			return fmt.Errorf("archive symlink %q: %w", name, err)
		}
	}
	return nil
}

func archiveExpansionRatioExceeded(expanded int64, compressed int64) bool {
	if expanded <= maximumArchiveExpansionSlack {
		return false
	}
	return compressed <= 0 || compressed <= expanded && expanded > compressed*maximumArchiveExpansionRatio
}

func walkTarGz(archivePath string, visit func(*tar.Header, *tar.Reader) error) error {
	file, err := os.Open(archivePath) // #nosec G304 -- generated staging path.
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer func() { _ = gzipReader.Close() }()
	reader := tar.NewReader(gzipReader)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := visit(header, reader); err != nil {
			return err
		}
	}
}

func cleanArchivePath(value string) (string, error) {
	if strings.Contains(value, "\\") {
		return "", errors.New("archive entry contains an ambiguous path separator")
	}
	clean := pathpkg.Clean(value)
	if !cleanArchiveRelativePath(clean) {
		return "", errors.New("archive entry escapes destination")
	}
	if clean != strings.TrimSuffix(value, "/") {
		return "", errors.New("archive entry path must be normalized")
	}
	return clean, nil
}

func validateArchiveTree(entries map[string]bool) error {
	for name := range entries {
		for ancestor := pathpkg.Dir(name); ancestor != "."; ancestor = pathpkg.Dir(ancestor) {
			if directory, exists := entries[ancestor]; exists && !directory {
				return fmt.Errorf("archive entry %q traverses file %q", name, ancestor)
			}
		}
	}
	return nil
}

func cleanTarLink(value string) (string, error) {
	if value == "" || strings.Contains(value, "\\") || pathpkg.IsAbs(value) ||
		filepath.IsAbs(filepath.FromSlash(value)) || filepath.VolumeName(filepath.FromSlash(value)) != "" {
		return "", errors.New("link target must be relative")
	}
	return pathpkg.Clean(value), nil
}

func cleanArchiveRelativePath(value string) bool {
	return value != "." && value != ".." && !pathpkg.IsAbs(value) &&
		!strings.HasPrefix(value, "../") && filepath.VolumeName(filepath.FromSlash(value)) == ""
}

func resolveTarSymlinks(value string, symlinks map[string]string) (string, error) {
	candidate := value
	visited := make(map[string]struct{}, len(symlinks))
	for {
		parts := strings.Split(candidate, "/")
		prefix := ""
		followed := false
		for index, part := range parts {
			prefix = pathpkg.Join(prefix, part)
			link, linked := symlinks[prefix]
			if !linked {
				continue
			}
			if _, seen := visited[prefix]; seen {
				return "", errors.New("symlink chain is cyclic")
			}
			visited[prefix] = struct{}{}
			suffix := strings.Join(parts[index+1:], "/")
			candidate = pathpkg.Clean(pathpkg.Join(pathpkg.Dir(prefix), link, suffix))
			if !cleanArchiveRelativePath(candidate) {
				return "", errors.New("symlink chain escapes destination")
			}
			followed = true
			break
		}
		if !followed {
			return candidate, nil
		}
	}
}

func archiveTarget(root string, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("archive entry escapes destination")
	}
	target := filepath.Join(root, clean)
	if !archivePathWithin(root, target) {
		return "", errors.New("archive entry escapes destination")
	}
	return target, nil
}

func archivePathWithin(root string, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func copyArchiveFile(path string, source io.Reader, mode os.FileMode, expectedSize int64) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm()&0o755) // #nosec G304 -- validated archive target.
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(source, expectedSize+1))
	if copyErr == nil && written != expectedSize {
		copyErr = fmt.Errorf("archive entry size mismatch: got %d, want %d", written, expectedSize)
	}
	closeErr := file.Close()
	return errors.Join(copyErr, closeErr)
}
