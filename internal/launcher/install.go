package launcher

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	central "github.com/cineko-org/contracts/v3"
	bootstrap "github.com/cineko-org/launcher/internal/keys"
)

const defaultArtifactDownloadTimeout = 10 * time.Minute

const (
	maximumArchiveEntries        = 100_000
	maximumArchiveFileSize       = int64(4 << 30)
	maximumArchiveExpandedSize   = int64(16 << 30)
	maximumArchiveExpansionRatio = int64(200)
	maximumArchiveExpansionSlack = int64(64 << 20)
	componentIntegrityFilename   = ".cineko-integrity.json"
)

type componentIntegrity struct {
	ArtifactSHA256 string `json:"artifactSha256"`
	TreeSHA256     string `json:"treeSha256"`
}

func installRelease(
	ctx context.Context,
	config Config,
	release central.RuntimeRelease,
) (installedRelease, error) {
	manifestPath := filepath.Join(config.DataDir, "runtime", "installed.json")
	previousPath := filepath.Join(config.DataDir, "runtime", "previous.json")
	if installed, err := loadInstalledRelease(config.DataDir, manifestPath, release); err == nil {
		if previous, err := loadInstalledManifest(config.DataDir, previousPath); err == nil {
			installed.Previous = &previous
		}
		report(config, Progress{Stage: StageInstalling, Message: "설치된 업데이트 검증 완료"})
		return installed, nil
	}
	previous, previousErr := loadInstalledManifest(config.DataDir, manifestPath)
	if retained, err := loadInstalledManifest(config.DataDir, previousPath); err == nil {
		previous, previousErr = retained, nil
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultArtifactDownloadTimeout}
	}
	paths, err := installArtifacts(ctx, config, client, release)
	if err != nil {
		return installedRelease{}, err
	}
	installed := installedRelease{
		Release: release, ClientPath: paths["client"], BrowserPath: paths["browser"],
		DriverPath: paths["playwright"],
	}
	installed.ProbePublicKeyHash, installed.ProbePublicKeySpec, err = installProbePublicKeys(
		filepath.Join(config.DataDir, "components", "keyring"),
		release.Client.ProbeBootstrapPublicKeys,
	)
	if err != nil {
		return installedRelease{}, err
	}
	if previousErr == nil && !sameRuntimeRelease(previous.Release, installed.Release) {
		if err := writeJSONAtomic(previousPath, previous); err != nil {
			return installedRelease{}, err
		}
		installed.Previous = &previous
	}
	if err := writeJSONAtomic(manifestPath, installed); err != nil {
		return installedRelease{}, err
	}
	report(config, Progress{Stage: StageInstalling, Message: "업데이트 설치 완료"})
	return installed, nil
}

type componentRelease struct {
	name     string
	artifact central.ReleaseArtifact
}

func installArtifacts(
	ctx context.Context,
	config Config,
	client *http.Client,
	release central.RuntimeRelease,
) (map[string]string, error) {
	items := []componentRelease{
		{name: "client", artifact: release.Client.Artifact},
		{name: "browser", artifact: release.Browser.Artifact},
		{name: "playwright", artifact: release.Playwright.Artifact},
	}
	total := release.Client.Artifact.Size + release.Browser.Artifact.Size + release.Playwright.Artifact.Size
	paths := make(map[string]string, len(items))
	var completed int64
	for _, item := range items {
		destination := filepath.Join(config.DataDir, "components", item.name, item.artifact.SHA256)
		if executable, err := loadInstalledComponent(destination, item); err == nil {
			paths[item.name] = executable
			completed += item.artifact.Size
			report(config, Progress{
				Stage: StageChecking, Message: "설치된 구성요소 확인 중", Artifact: item.name,
				Downloaded: completed, Total: total,
			})
			continue
		}
		report(config, Progress{
			Stage: StageDownloading, Message: "업데이트 다운로드 중", Artifact: item.name,
			Downloaded: completed, Total: total,
		})
		archivePath, err := downloadArtifact(
			ctx, client, filepath.Join(config.DataDir, "downloads"), item.name, item.artifact,
			func(downloaded int64) {
				report(config, Progress{
					Stage: StageDownloading, Message: "업데이트 다운로드 중", Artifact: item.name,
					Downloaded: completed + downloaded, Total: total,
				})
			},
		)
		if err != nil {
			return nil, err
		}
		report(config, Progress{Stage: StageInstalling, Message: "다운로드 검증 및 설치 중", Artifact: item.name})
		executable, err := activateComponent(archivePath, destination, item)
		if err != nil {
			return nil, fmt.Errorf("install %s artifact: %w", item.name, err)
		}
		paths[item.name] = executable
		completed += item.artifact.Size
		_ = os.Remove(archivePath)
	}
	return paths, nil
}

func loadInstalledComponent(root string, item componentRelease) (string, error) {
	if err := validateManagedDirectory(filepath.Dir(filepath.Dir(root)), root); err != nil {
		return "", err
	}
	if err := validateComponentIntegrity(root, item.artifact.SHA256); err != nil {
		return "", err
	}
	executable := filepath.Join(root, filepath.FromSlash(item.artifact.Executable))
	if err := requireExecutable(executable); err != nil {
		return "", err
	}
	if item.name == "playwright" {
		if err := requirePlaywrightDriver(root); err != nil {
			return "", err
		}
		return root, nil
	}
	return executable, nil
}

func activateComponent(archivePath string, root string, item componentRelease) (string, error) {
	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	if err := validateManagedDirectory(filepath.Dir(filepath.Dir(parent)), parent); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(parent, ".install-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := extractArtifact(archivePath, item.artifact.URL, staging); err != nil {
		return "", err
	}
	if err := writeComponentIntegrity(staging, item.artifact.SHA256); err != nil {
		return "", fmt.Errorf("record %s integrity: %w", item.name, err)
	}
	if _, err := loadInstalledComponent(staging, item); err != nil {
		return "", fmt.Errorf("validate %s executable: %w", item.name, err)
	}
	if err := removeManagedEntry(root); err != nil {
		return "", err
	}
	if err := os.Rename(staging, root); err != nil {
		return "", err
	}
	return loadInstalledComponent(root, item)
}

func writeComponentIntegrity(root string, artifactSHA256 string) error {
	treeSHA256, err := componentTreeSHA256(root)
	if err != nil {
		return err
	}
	return writeJSONAtomic(filepath.Join(root, componentIntegrityFilename), componentIntegrity{
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
		if entries > maximumArchiveEntries {
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
		if info.Size() > maximumArchiveFileSize {
			return errors.New("component file exceeds size limit")
		}
		if info.Size() > maximumArchiveExpandedSize-*expanded {
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

func loadInstalledRelease(
	dataDir string,
	manifestPath string,
	release central.RuntimeRelease,
) (installedRelease, error) {
	installed, err := loadInstalledManifest(dataDir, manifestPath)
	if err != nil {
		return installedRelease{}, err
	}
	if !sameRuntimeRelease(installed.Release, release) {
		return installedRelease{}, errors.New("installed runtime manifest does not match release")
	}
	return installed, nil
}

func loadInstalledManifest(dataDir string, manifestPath string) (installedRelease, error) {
	contents, err := os.ReadFile(manifestPath) // #nosec G304 -- scoped runtime manifest.
	if err != nil {
		return installedRelease{}, err
	}
	var installed installedRelease
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&installed); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return installedRelease{}, errors.New("installed runtime manifest is invalid")
	}
	if err := validateInstalledExecutables(dataDir, installed); err != nil {
		return installedRelease{}, err
	}
	if err := validateInstalledPublicKeys(
		dataDir, installed.ProbePublicKeyHash, installed.ProbePublicKeySpec,
		installed.Release.Client.ProbeBootstrapPublicKeys,
	); err != nil {
		return installedRelease{}, err
	}
	return installed, nil
}

func sameRuntimeRelease(left central.RuntimeRelease, right central.RuntimeRelease) bool {
	return left.Client.Version == right.Client.Version &&
		left.Client.Protocol == right.Client.Protocol &&
		left.Client.Artifact.SHA256 == right.Client.Artifact.SHA256 &&
		left.Browser.Revision == right.Browser.Revision &&
		left.Browser.Artifact.SHA256 == right.Browser.Artifact.SHA256 &&
		left.Playwright.Version == right.Playwright.Version &&
		left.Playwright.Artifact.SHA256 == right.Playwright.Artifact.SHA256 &&
		maps.Equal(left.Client.ProbeBootstrapPublicKeys, right.Client.ProbeBootstrapPublicKeys)
}

func rollbackInstalledRelease(dataDir string, installed installedRelease) error {
	if installed.Previous == nil {
		return nil
	}
	if err := validateInstalledExecutables(dataDir, *installed.Previous); err != nil {
		return err
	}
	if err := validateInstalledPublicKeys(
		dataDir,
		installed.Previous.ProbePublicKeyHash,
		installed.Previous.ProbePublicKeySpec,
		installed.Previous.Release.Client.ProbeBootstrapPublicKeys,
	); err != nil {
		return err
	}
	if err := writeJSONAtomic(filepath.Join(dataDir, "runtime", "installed.json"), installed.Previous); err != nil {
		return err
	}
	return os.Remove(filepath.Join(dataDir, "runtime", "previous.json"))
}

func finalizeInstalledRelease(dataDir string, installed installedRelease) {
	if err := os.Remove(filepath.Join(dataDir, "runtime", "previous.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
	cleanupRuntime(dataDir, installed)
}

func validateInstalledExecutables(dataDir string, installed installedRelease) error {
	componentsRoot := filepath.Join(dataDir, "components")
	items := []struct {
		name       string
		artifact   central.ReleaseArtifact
		storedPath string
	}{
		{name: "client", artifact: installed.Release.Client.Artifact, storedPath: installed.ClientPath},
		{name: "browser", artifact: installed.Release.Browser.Artifact, storedPath: installed.BrowserPath},
		{name: "playwright", artifact: installed.Release.Playwright.Artifact, storedPath: installed.DriverPath},
	}
	for _, item := range items {
		root := filepath.Join(componentsRoot, item.name, item.artifact.SHA256)
		executable, err := loadInstalledComponent(root, componentRelease{name: item.name, artifact: item.artifact})
		if err != nil {
			return fmt.Errorf("validate installed %s: %w", item.name, err)
		}
		if filepath.Clean(item.storedPath) != executable {
			return fmt.Errorf("installed %s path does not match its content-addressed component", item.name)
		}
	}
	return nil
}

func validateInstalledPublicKeys(
	dataDir string,
	digest string,
	spec string,
	keyring map[string]string,
) error {
	expectedDigest, keyIDs, err := validateProbePublicKeySet(keyring)
	if err != nil || !strings.EqualFold(digest, expectedDigest) {
		return errors.New("installed Probe public-key digest is invalid")
	}
	directory := filepath.Join(dataDir, "components", "keyring", expectedDigest)
	if err := validateManagedDirectory(filepath.Join(dataDir, "components", "keyring"), directory); err != nil {
		return err
	}
	expectedEntries := make([]string, 0, len(keyIDs))
	for _, keyID := range keyIDs {
		keyPath := filepath.Join(directory, keyID+".pem")
		expectedEntries = append(expectedEntries, keyID+"="+keyPath)
		info, err := os.Lstat(keyPath)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("installed Probe public key %q is not a regular file", keyID)
		}
		contents, err := os.ReadFile(keyPath) // #nosec G304 -- content-addressed keyring path.
		if err != nil || string(contents) != keyring[keyID] {
			return fmt.Errorf("installed Probe public key %q is invalid", keyID)
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != len(keyIDs) || spec != strings.Join(expectedEntries, ",") {
		return errors.New("installed Probe public-key set is invalid")
	}
	return nil
}

func validateManagedDirectory(root string, candidate string) error {
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

func installProbePublicKeys(root string, keyring map[string]string) (string, string, error) {
	digest, keyIDs, err := validateProbePublicKeySet(keyring)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", "", fmt.Errorf("create Probe public-key root: %w", err)
	}
	if err := validateManagedDirectory(filepath.Dir(filepath.Dir(root)), root); err != nil {
		return "", "", err
	}
	directory := filepath.Join(root, digest)
	if _, err := os.Stat(directory); err == nil {
		spec := probePublicKeySpec(directory, keyIDs)
		if err := validateInstalledPublicKeys(filepath.Dir(filepath.Dir(root)), digest, spec, keyring); err != nil {
			return "", "", err
		}
		return digest, spec, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	staging, err := os.MkdirTemp(root, ".install-*")
	if err != nil {
		return "", "", err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	for _, keyID := range keyIDs {
		path := filepath.Join(staging, keyID+".pem")
		if err := os.WriteFile(path, []byte(keyring[keyID]), 0o600); err != nil {
			return "", "", fmt.Errorf("write Probe public key %q: %w", keyID, err)
		}
	}
	if err := os.Rename(staging, directory); err != nil {
		return "", "", err
	}
	return digest, probePublicKeySpec(directory, keyIDs), nil
}

func validateProbePublicKeySet(keyring map[string]string) (string, []string, error) {
	if len(keyring) == 0 || len(keyring) > 16 {
		return "", nil, errors.New("probe public-key set must contain 1 to 16 keys")
	}
	keyIDs := make([]string, 0, len(keyring))
	for keyID := range keyring {
		keyIDs = append(keyIDs, keyID)
	}
	sort.Strings(keyIDs)
	hasher := sha256.New()
	for _, keyID := range keyIDs {
		if !validProbeKeyID(keyID) {
			return "", nil, fmt.Errorf("invalid Probe public key ID %q", keyID)
		}
		contents := []byte(keyring[keyID])
		if _, err := bootstrap.ParsePublicKeyPEM(contents); err != nil {
			return "", nil, fmt.Errorf("validate Probe public key %q: %w", keyID, err)
		}
		_, _ = fmt.Fprintf(hasher, "%s\x00%d\x00", keyID, len(contents))
		_, _ = hasher.Write(contents)
	}
	return hex.EncodeToString(hasher.Sum(nil)), keyIDs, nil
}

func probePublicKeySpec(directory string, keyIDs []string) string {
	entries := make([]string, 0, len(keyIDs))
	for _, keyID := range keyIDs {
		entries = append(entries, keyID+"="+filepath.Join(directory, keyID+".pem"))
	}
	return strings.Join(entries, ",")
}

func validProbeKeyID(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func downloadArtifact(
	ctx context.Context,
	client *http.Client,
	directory string,
	name string,
	artifact central.ReleaseArtifact,
	onProgress func(int64),
) (string, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create artifact cache: %w", err)
	}
	if err := validateManagedDirectory(filepath.Dir(directory), directory); err != nil {
		return "", fmt.Errorf("validate artifact cache: %w", err)
	}
	finalPath := filepath.Join(directory, strings.ToLower(artifact.SHA256)+".blob")
	partialPath := finalPath + ".part"
	if fileMatchesArtifact(finalPath, artifact) {
		if onProgress != nil {
			onProgress(artifact.Size)
		}
		return finalPath, nil
	}
	_ = os.Remove(finalPath)
	if fileMatchesArtifact(partialPath, artifact) {
		if err := os.Rename(partialPath, finalPath); err != nil {
			return "", fmt.Errorf("activate %s completed partial artifact: %w", name, err)
		}
		if onProgress != nil {
			onProgress(artifact.Size)
		}
		return finalPath, nil
	}
	if size, err := regularFileSize(partialPath); err != nil || size >= artifact.Size {
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

func DownloadPortableLauncher(
	ctx context.Context,
	client *http.Client,
	cacheDir string,
	artifact central.ReleaseArtifact,
	destination string,
	progress func(int64),
) error {
	if err := validateArtifactMetadata(artifact); err != nil {
		return fmt.Errorf("validate Launcher artifact: %w", err)
	}
	if client == nil {
		client = &http.Client{Timeout: defaultArtifactDownloadTimeout}
	}
	cached, err := downloadArtifact(ctx, client, cacheDir, "launcher", artifact, progress)
	if err != nil {
		return err
	}
	return copyPortableLauncher(cached, destination, artifact)
}

func copyPortableLauncher(cached string, destination string, artifact central.ReleaseArtifact) error {
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
	if _, err := io.CopyN(temporary, source, artifact.Size); err != nil {
		_ = temporary.Close()
		return err
	}
	if extra, err := source.Read(make([]byte, 1)); err != io.EOF || extra != 0 {
		_ = temporary.Close()
		return errors.New("verified Launcher artifact size changed while copying")
	}
	mode := os.FileMode(0o600)
	if strings.HasSuffix(strings.ToLower(artifact.URL), ".appimage") {
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
	return replaceFileAtomic(temporaryPath, destination)
}

func resumeArtifactDownload(
	ctx context.Context,
	client *http.Client,
	partialPath string,
	finalPath string,
	name string,
	artifact central.ReleaseArtifact,
	onProgress func(int64),
) (string, bool, error) {
	offset, err := regularFileSize(partialPath)
	if err != nil || offset > artifact.Size {
		return "", true, nil
	}
	request, err := artifactRequest(ctx, artifact.URL, offset)
	if err != nil {
		return "", false, fmt.Errorf("create %s artifact request: %w", name, err)
	}
	response, err := client.Do(request)
	if err != nil {
		return "", false, fmt.Errorf("download %s artifact: %w", name, err)
	}
	defer func() { _ = response.Body.Close() }()
	if restart, err := validateArtifactResponse(response, offset, artifact.Size); restart || err != nil {
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
	artifact central.ReleaseArtifact,
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
	progress := &artifactProgressWriter{written: offset, total: artifact.Size, notify: onProgress}
	written, copyErr := io.Copy(
		io.MultiWriter(file, hasher, progress),
		io.LimitReader(response.Body, artifact.Size-offset+1),
	)
	if copyErr != nil {
		return fmt.Errorf("write %s artifact: %w", name, copyErr)
	}
	if offset+written != artifact.Size || !strings.EqualFold(hex.EncodeToString(hasher.Sum(nil)), artifact.SHA256) {
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

func fileMatchesArtifact(path string, artifact central.ReleaseArtifact) bool {
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
	if err != nil || !os.SameFile(identity, info) || !info.Mode().IsRegular() || info.Size() != artifact.Size {
		return false
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return false
	}
	return strings.EqualFold(hex.EncodeToString(hasher.Sum(nil)), artifact.SHA256)
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

func cleanupRuntime(dataDir string, installed installedRelease) {
	active := map[string]string{
		"client":     installed.Release.Client.Artifact.SHA256,
		"browser":    installed.Release.Browser.Artifact.SHA256,
		"playwright": installed.Release.Playwright.Artifact.SHA256,
		"keyring":    installed.ProbePublicKeyHash,
	}
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
	_ = writeJSONAtomic(pending, failed)
}

func cleanupDirectoryEntries(managedRoot string, root string, keep string, failed *[]string) {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() ||
		validateManagedDirectory(managedRoot, root) != nil {
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
		if err := removeManagedEntry(candidate); err != nil {
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
		validateManagedDirectory(managedRoot, root) != nil {
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
		if err := removeManagedEntry(candidate); err != nil {
			*failed = append(*failed, candidate)
		}
	}
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

func removeManagedEntry(path string) error {
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

func extractArtifact(archivePath string, sourceURL string, destination string) error {
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
	if len(files) > maximumArchiveEntries {
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
	if err != nil || size > maximumArchiveFileSize {
		return "", fmt.Errorf("archive entry %q exceeds size limit", name)
	}
	if size > maximumArchiveExpandedSize-*expanded {
		return "", errors.New("archive expanded size exceeds limit")
	}
	*expanded += size
	compressedSize, err := checkedArchiveSize(entry.CompressedSize64)
	if err != nil || compressedSize > maximumArchiveExpandedSize {
		return "", errors.New("archive compressed size exceeds limit")
	}
	if compressedSize > maximumArchiveExpandedSize-*compressed {
		*compressed = maximumArchiveExpandedSize
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
	if len(index.entries) >= maximumArchiveEntries {
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
		if header.Size < 0 || header.Size > maximumArchiveFileSize {
			return fmt.Errorf("archive entry %q exceeds size limit", name)
		}
		if header.Size > maximumArchiveExpandedSize-*expanded {
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
	if !pathWithin(root, target) {
		return "", errors.New("archive entry escapes destination")
	}
	return target, nil
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

func pathWithin(root string, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func writeJSONAtomic(path string, value any) error {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
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
	if err := replaceFileAtomic(temporary, path); err != nil {
		return fmt.Errorf("activate %s: %w", filepath.Base(path), err)
	}
	return nil
}
