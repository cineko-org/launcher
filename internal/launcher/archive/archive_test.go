package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestArchiveExpansionLimits(t *testing.T) {
	regular := func(name string, size uint64) *zip.File {
		entry := &zip.File{FileHeader: zip.FileHeader{Name: name, UncompressedSize64: size}}
		entry.SetMode(0o600)
		return entry
	}
	if _, err := inspectZip(
		[]*zip.File{regular("oversized", uint64(MaximumFileSize)+1)}, t.TempDir(),
	); err == nil {
		t.Fatal("oversized archive entry accepted")
	}
	if _, err := inspectZip([]*zip.File{
		regular("first", uint64(MaximumFileSize)),
		regular("second", uint64(MaximumFileSize)),
		regular("third", uint64(MaximumFileSize)),
		regular("fourth", uint64(MaximumFileSize)),
		regular("overflow", 1),
	}, t.TempDir()); err == nil {
		t.Fatal("oversized expanded archive accepted")
	}
	entries := make([]*zip.File, MaximumEntries+1)
	if _, err := inspectZip(entries, t.TempDir()); err == nil {
		t.Fatal("archive entry-count limit was not enforced")
	}
	ratioBomb := regular("ratio-bomb", uint64(maximumArchiveExpansionSlack+1))
	ratioBomb.CompressedSize64 = uint64((maximumArchiveExpansionSlack + 1) / maximumArchiveExpansionRatio)
	if _, err := inspectZip([]*zip.File{ratioBomb}, t.TempDir()); err == nil {
		t.Fatal("archive expansion-ratio limit was not enforced")
	}
}

func TestExtractRejectsEscapesAndAbsolutePaths(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.Create("../escape")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = entry.Write([]byte("bad"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bad.zip")
	if err := os.WriteFile(path, archive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractZip(path, t.TempDir()); err == nil {
		t.Fatal("escaping ZIP entry accepted")
	}
	if _, err := archiveTarget(t.TempDir(), "/absolute"); err == nil {
		t.Fatal("absolute archive entry accepted")
	}
}

type tarFixtureEntry struct {
	name     string
	typeflag byte
	linkname string
	contents string
}

func TestTarExtractionAllowsSafeRelativeSymlinkChain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires host policy on Windows")
	}
	archive := tarGzFixture(t, []tarFixtureEntry{
		{name: "lib/client", typeflag: tar.TypeReg, contents: "client"},
		{name: "links/client", typeflag: tar.TypeSymlink, linkname: "../lib/client"},
		{name: "aliases/client", typeflag: tar.TypeSymlink, linkname: "../links/client"},
	})
	destination := t.TempDir()
	if err := extractTarGz(archive, destination); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(destination, "aliases", "client")) // #nosec G304 -- test-owned path.
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "client" {
		t.Fatalf("linked contents = %q", contents)
	}
}

func TestTarExtractionRejectsUnsafeLinksBeforeWriting(t *testing.T) {
	tests := map[string][]tarFixtureEntry{
		"absolute target": {
			{name: "client", typeflag: tar.TypeSymlink, linkname: "/tmp/outside"},
		},
		"escaping target": {
			{name: "bin/client", typeflag: tar.TypeSymlink, linkname: "../../outside"},
		},
		"cyclic chain": {
			{name: "first", typeflag: tar.TypeSymlink, linkname: "second"},
			{name: "second", typeflag: tar.TypeSymlink, linkname: "first"},
		},
		"self-referencing link": {
			{name: "client", typeflag: tar.TypeSymlink, linkname: "client"},
		},
		"symlink parent chain": {
			{name: "alias", typeflag: tar.TypeSymlink, linkname: "real"},
			{name: "alias/payload", typeflag: tar.TypeReg, contents: "outside"},
		},
		"hard link": {
			{name: "client", typeflag: tar.TypeLink, linkname: "target"},
		},
	}
	for name, entries := range tests {
		t.Run(name, func(t *testing.T) {
			destination := t.TempDir()
			if err := extractTarGz(tarGzFixture(t, entries), destination); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			contents, err := os.ReadDir(destination)
			if err != nil {
				t.Fatal(err)
			}
			if len(contents) != 0 {
				t.Fatalf("archive wrote %d entries before validation failed", len(contents))
			}
		})
	}
}

func TestZipExtractionAllowsMacFrameworkSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires host policy on Windows")
	}
	archive := zipFixture(t, []tarFixtureEntry{
		{name: "Chrome.framework/Versions/A/Resources/data", typeflag: tar.TypeReg, contents: "resource"},
		{name: "Chrome.framework/Versions/Current", typeflag: tar.TypeSymlink, linkname: "A"},
		{name: "Chrome.framework/Resources", typeflag: tar.TypeSymlink, linkname: "Versions/Current/Resources"},
	})
	destination := t.TempDir()
	if err := extractZip(archive, destination); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(destination, "Chrome.framework", "Resources", "data")) // #nosec G304 -- test-owned path.
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "resource" {
		t.Fatalf("linked contents = %q", contents)
	}
}

func TestZipExtractionRejectsUnsafeLinksBeforeWriting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires host policy on Windows")
	}
	tests := map[string][]tarFixtureEntry{
		"absolute target": {
			{name: "client", typeflag: tar.TypeSymlink, linkname: "/tmp/outside"},
		},
		"escaping target": {
			{name: "bin/client", typeflag: tar.TypeSymlink, linkname: "../../outside"},
		},
		"cyclic chain": {
			{name: "first", typeflag: tar.TypeSymlink, linkname: "second"},
			{name: "second", typeflag: tar.TypeSymlink, linkname: "first"},
		},
		"symlink parent chain": {
			{name: "alias", typeflag: tar.TypeSymlink, linkname: "real"},
			{name: "alias/payload", typeflag: tar.TypeReg, contents: "outside"},
		},
	}
	for name, entries := range tests {
		t.Run(name, func(t *testing.T) {
			destination := t.TempDir()
			if err := extractZip(zipFixture(t, entries), destination); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			contents, err := os.ReadDir(destination)
			if err != nil {
				t.Fatal(err)
			}
			if len(contents) != 0 {
				t.Fatalf("archive wrote %d entries before validation failed", len(contents))
			}
		})
	}
}

func zipFixture(t *testing.T, entries []tarFixtureEntry) string {
	t.Helper()
	var contents bytes.Buffer
	writer := zip.NewWriter(&contents)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		switch entry.typeflag {
		case tar.TypeDir:
			header.Name += "/"
			header.SetMode(os.ModeDir | 0o755)
		case tar.TypeSymlink:
			header.SetMode(os.ModeSymlink | 0o777)
			entry.contents = entry.linkname
		default:
			header.SetMode(0o755)
		}
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(entry.contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fixture.zip")
	if err := os.WriteFile(path, contents.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func tarGzFixture(t *testing.T, entries []tarFixtureEntry) string {
	t.Helper()
	var contents bytes.Buffer
	gzipWriter := gzip.NewWriter(&contents)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		header := &tar.Header{
			Name: entry.name, Typeflag: entry.typeflag, Linkname: entry.linkname,
			Mode: 0o755, Size: int64(len(entry.contents)),
		}
		if entry.typeflag != tar.TypeReg {
			header.Size = 0
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := tarWriter.Write([]byte(entry.contents)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fixture.tar.gz")
	if err := os.WriteFile(path, contents.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
