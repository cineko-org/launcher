package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	releasepb "github.com/cineko-org/contracts/gen/go/cineko/release"
	"github.com/cineko-org/launcher/internal/launcher/managedfiles"
)

// DefaultDownloadTimeout bounds an artifact request when no client is supplied.
const DefaultDownloadTimeout = 10 * time.Minute

// ValidateMetadata checks the immutable URL, digest, size, and archive path
// metadata received from Central before it is used by the Launcher.
func ValidateMetadata(artifact *releasepb.Artifact) error {
	if artifact == nil {
		return errors.New("artifact is required")
	}
	parsed, err := url.Parse(strings.TrimSpace(artifact.GetUrl()))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" ||
		artifact.GetSize() <= 0 {
		return errors.New("HTTPS URL and positive size are required")
	}
	digest, err := hex.DecodeString(strings.TrimSpace(artifact.GetSha256()))
	if err != nil || len(digest) != sha256.Size {
		return errors.New("SHA-256 must contain 64 hexadecimal characters")
	}
	executable := strings.TrimSpace(artifact.GetExecutable())
	if executable == "" || pathpkg.IsAbs(executable) || pathpkg.Clean(executable) != executable ||
		strings.HasPrefix(executable, "../") {
		return errors.New("executable must be a clean relative archive path")
	}
	return nil
}

// Download acquires an HTTPS release artifact into a digest-scoped cache.
func Download(
	ctx context.Context,
	client *http.Client,
	directory string,
	name string,
	artifact *releasepb.Artifact,
	onProgress func(int64),
) (string, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create artifact cache: %w", err)
	}
	if err := managedfiles.ValidateDirectory(filepath.Dir(directory), directory); err != nil {
		return "", fmt.Errorf("validate artifact cache: %w", err)
	}
	finalPath := filepath.Join(directory, strings.ToLower(artifact.GetSha256())+".blob")
	partialPath := finalPath + ".part"
	if fileMatchesArtifact(finalPath, artifact) {
		if onProgress != nil {
			onProgress(artifact.GetSize())
		}
		return finalPath, nil
	}
	_ = os.Remove(finalPath)
	if fileMatchesArtifact(partialPath, artifact) {
		if err := os.Rename(partialPath, finalPath); err != nil {
			return "", fmt.Errorf("activate %s completed partial artifact: %w", name, err)
		}
		if onProgress != nil {
			onProgress(artifact.GetSize())
		}
		return finalPath, nil
	}
	if size, err := regularFileSize(partialPath); err != nil || size >= artifact.GetSize() {
		_ = os.RemoveAll(partialPath)
	}
	for attempt := 0; attempt < 2; attempt++ {
		path, restart, err := resumeArtifactDownload(
			ctx, client, partialPath, finalPath, name, artifact, onProgress,
		)
		if !restart {
			return path, err
		}
		_ = os.RemoveAll(partialPath)
	}
	return "", fmt.Errorf("download %s artifact: server did not honor resume request", name)
}

// DownloadPortableLauncher verifies and atomically places a portable Launcher.
func DownloadPortableLauncher(
	ctx context.Context,
	client *http.Client,
	cacheDir string,
	artifact *releasepb.Artifact,
	destination string,
	progress func(int64),
) error {
	if err := ValidateMetadata(artifact); err != nil {
		return fmt.Errorf("validate Launcher artifact: %w", err)
	}
	if client == nil {
		client = &http.Client{Timeout: DefaultDownloadTimeout}
	}
	cached, err := Download(ctx, client, cacheDir, "launcher", artifact, progress)
	if err != nil {
		return err
	}
	return copyPortableLauncher(cached, destination, artifact)
}

func copyPortableLauncher(cached string, destination string, artifact *releasepb.Artifact) error {
	destination = filepath.Clean(destination)
	if destination == "." || destination == string(filepath.Separator) {
		return errors.New("launcher destination is invalid")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	source, err := os.Open(cached) // #nosec G304 -- verified content-addressed cache path.
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".cineko-launcher-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := io.CopyN(temporary, source, artifact.GetSize()); err != nil {
		_ = temporary.Close()
		return err
	}
	if extra, err := source.Read(make([]byte, 1)); err != io.EOF || extra != 0 {
		_ = temporary.Close()
		return errors.New("verified Launcher artifact size changed while copying")
	}
	mode := os.FileMode(0o600)
	if strings.HasSuffix(strings.ToLower(artifact.GetUrl()), ".appimage") {
		mode = 0o700
	}
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return managedfiles.ReplaceAtomic(temporaryPath, destination)
}

func resumeArtifactDownload(
	ctx context.Context,
	client *http.Client,
	partialPath string,
	finalPath string,
	name string,
	artifact *releasepb.Artifact,
	onProgress func(int64),
) (string, bool, error) {
	offset, err := regularFileSize(partialPath)
	if err != nil || offset > artifact.GetSize() {
		return "", true, nil
	}
	request, err := artifactRequest(ctx, artifact.GetUrl(), offset)
	if err != nil {
		return "", false, fmt.Errorf("create %s artifact request: %w", name, err)
	}
	response, err := client.Do(request)
	if err != nil {
		return "", false, fmt.Errorf("download %s artifact: %w", name, err)
	}
	defer func() { _ = response.Body.Close() }()
	if restart, err := validateArtifactResponse(response, offset, artifact.GetSize()); restart || err != nil {
		if err != nil {
			return "", false, fmt.Errorf("download %s artifact: %w", name, err)
		}
		return "", true, nil
	}
	if err := appendArtifactResponse(response, partialPath, name, artifact, offset, onProgress); err != nil {
		return "", false, err
	}
	if err := os.Rename(partialPath, finalPath); err != nil {
		return "", false, fmt.Errorf("activate %s artifact cache: %w", name, err)
	}
	return finalPath, false, nil
}

func appendArtifactResponse(
	response *http.Response,
	partialPath string,
	name string,
	artifact *releasepb.Artifact,
	offset int64,
	onProgress func(int64),
) error {
	file, err := openArtifactPartial(partialPath)
	if err != nil {
		return fmt.Errorf("open %s partial artifact: %w", name, err)
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	if offset > 0 {
		if _, err := io.Copy(hasher, io.LimitReader(file, offset)); err != nil {
			return fmt.Errorf("hash %s partial artifact: %w", name, err)
		}
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(offset)
	}
	progress := &artifactProgressWriter{written: offset, total: artifact.GetSize(), notify: onProgress}
	written, copyErr := io.Copy(
		io.MultiWriter(file, hasher, progress),
		io.LimitReader(response.Body, artifact.GetSize()-offset+1),
	)
	if copyErr != nil {
		return fmt.Errorf("write %s artifact: %w", name, copyErr)
	}
	if offset+written != artifact.GetSize() || !strings.EqualFold(hex.EncodeToString(hasher.Sum(nil)), artifact.GetSha256()) {
		return fmt.Errorf("verify %s artifact: size or SHA-256 mismatch", name)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync %s artifact: %w", name, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s artifact: %w", name, err)
	}
	return nil
}

func openArtifactPartial(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600) // #nosec G304 -- digest-scoped cache path.
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("partial artifact is not a regular file")
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0o600) // #nosec G304 -- identity checked against Lstat below.
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, errors.New("partial artifact changed while opening")
	}
	return file, nil
}

func artifactRequest(ctx context.Context, url string, offset int64) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err == nil && offset > 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	return request, err
}

func validateArtifactResponse(response *http.Response, offset int64, size int64) (bool, error) {
	if response.Request == nil || response.Request.URL.Scheme != "https" {
		return false, errors.New("insecure redirect")
	}
	if offset == 0 {
		if response.StatusCode != http.StatusOK {
			return false, fmt.Errorf("unexpected HTTP %d", response.StatusCode)
		}
		return false, nil
	}
	if response.StatusCode == http.StatusOK || response.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return true, nil
	}
	if response.StatusCode != http.StatusPartialContent {
		return false, fmt.Errorf("unexpected HTTP %d", response.StatusCode)
	}
	return !validContentRange(response.Header.Get("Content-Range"), offset, size), nil
}

type artifactProgressWriter struct {
	written int64
	total   int64
	notify  func(int64)
}

func (writer *artifactProgressWriter) Write(contents []byte) (int, error) {
	writer.written += int64(len(contents))
	if writer.notify != nil {
		writer.notify(min(writer.written, writer.total))
	}
	return len(contents), nil
}

func regularFileSize(path string) (int64, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return 0, errors.New("partial artifact is not a regular file")
	}
	return info.Size(), nil
}

func fileMatchesArtifact(path string, artifact *releasepb.Artifact) bool {
	identity, err := os.Lstat(path)
	if err != nil || identity.Mode()&os.ModeSymlink != 0 || !identity.Mode().IsRegular() {
		return false
	}
	file, err := os.Open(path) // #nosec G304 -- digest-scoped cache path.
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !os.SameFile(identity, info) || !info.Mode().IsRegular() || info.Size() != artifact.GetSize() {
		return false
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return false
	}
	return strings.EqualFold(hex.EncodeToString(hasher.Sum(nil)), artifact.GetSha256())
}

func validContentRange(value string, offset int64, size int64) bool {
	unit, bounds, ok := strings.Cut(strings.TrimSpace(value), " ")
	if !ok || unit != "bytes" {
		return false
	}
	span, totalValue, ok := strings.Cut(bounds, "/")
	if !ok {
		return false
	}
	startValue, endValue, ok := strings.Cut(span, "-")
	if !ok {
		return false
	}
	start, startErr := strconv.ParseInt(startValue, 10, 64)
	end, endErr := strconv.ParseInt(endValue, 10, 64)
	total, totalErr := strconv.ParseInt(totalValue, 10, 64)
	return startErr == nil && endErr == nil && totalErr == nil &&
		start == offset && end == size-1 && total == size
}
