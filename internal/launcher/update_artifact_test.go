package launcher

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	central "github.com/cineko-org/contracts/v3"
)

func TestProbePublicKeyIDsCannotEscapeInstallDirectory(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	root := filepath.Join(t.TempDir(), "components", "keyring")
	for _, keyID := range []string{"../outside", "nested/key", ".", ""} {
		if _, _, err := installProbePublicKeys(root, map[string]string{keyID: publicKey}); err == nil {
			t.Fatalf("unsafe key ID %q accepted", keyID)
		}
	}
	digest, spec, err := installProbePublicKeys(root, map[string]string{"primary-2026_08": publicKey})
	if err != nil {
		t.Fatalf("valid key ID rejected: %v", err)
	}
	if !strings.Contains(spec, filepath.Join("keyring", digest, "primary-2026_08.pem")) {
		t.Fatalf("keyring spec = %q", spec)
	}
}

func TestArtifactDownloadResumesAndReusesVerifiedCache(t *testing.T) {
	payload := bytes.Repeat([]byte("cineko-range-fixture"), 64*1024)
	offset := int64(len(payload) / 2)
	var requests atomic.Int32
	var transferred atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Header.Get("Authorization") != "" {
			t.Error("public CDN request included Central credentials")
		}
		if got := request.Header.Get("Range"); got != fmt.Sprintf("bytes=%d-", offset) {
			t.Errorf("Range=%q", got)
		}
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(payload)-1, len(payload)))
		writer.WriteHeader(http.StatusPartialContent)
		written, _ := writer.Write(payload[offset:])
		transferred.Add(int64(written))
	}))
	t.Cleanup(server.Close)
	digest := sha256.Sum256(payload)
	artifact := central.ReleaseArtifact{
		URL: server.URL + "/cineko-client.zip", Size: int64(len(payload)),
		SHA256: hex.EncodeToString(digest[:]), Executable: "cineko-client",
	}
	cache := t.TempDir()
	partial := filepath.Join(cache, artifact.SHA256+".blob.part")
	if err := os.WriteFile(partial, payload[:offset], 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := downloadArtifact(t.Context(), server.Client(), cache, "client", artifact, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !fileMatchesArtifact(path, artifact) {
		t.Fatal("resumed artifact did not pass digest verification")
	}
	if got, want := transferred.Load(), int64(len(payload))-offset; got != want {
		t.Fatalf("transferred=%d, want=%d", got, want)
	}
	if _, err := downloadArtifact(t.Context(), server.Client(), cache, "client", artifact, nil); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("verified cache made %d network requests", requests.Load())
	}
	baselineRetry := int64(len(payload))
	saved := baselineRetry - transferred.Load()
	t.Logf(
		"Range resume transferred %d bytes instead of %d bytes for the retry (saved %d bytes, %.1f%%)",
		transferred.Load(), baselineRetry, saved, float64(saved)*100/float64(baselineRetry),
	)
}

func TestInstalledComponentIntegrityDetectsMutation(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "client")
	writeExecutableFixture(t, executable, []byte("original"))
	digest := strings.Repeat("a", 64)
	if err := writeComponentIntegrity(root, digest); err != nil {
		t.Fatal(err)
	}
	item := componentRelease{name: "client", artifact: central.ReleaseArtifact{
		SHA256: digest, Executable: "client",
	}}
	if _, err := loadInstalledComponent(root, item); err != nil {
		t.Fatal(err)
	}
	writeExecutableFixture(t, executable, []byte("modified"))
	if _, err := loadInstalledComponent(root, item); err == nil {
		t.Fatal("mutated installed component passed integrity validation")
	}
}

func TestInstalledManifestRejectsMutatedComponentTree(t *testing.T) {
	dataDir := t.TempDir()
	installed := installedFixture(t, dataDir, "a")
	manifest := filepath.Join(dataDir, "runtime", "installed.json")
	if err := writeJSONAtomic(manifest, installed); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInstalledManifest(dataDir, manifest); err != nil {
		t.Fatalf("valid installed manifest rejected: %v", err)
	}
	writeExecutableFixture(t, installed.BrowserPath, []byte("tampered"))
	if _, err := loadInstalledManifest(dataDir, manifest); err == nil {
		t.Fatal("installed manifest accepted a mutated browser component")
	}
}

func TestWriteJSONAtomicReplacesExistingManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installed.json")
	if err := writeJSONAtomic(path, map[string]int{"generation": 1}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(path, map[string]int{"generation": 2}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path) // #nosec G304 -- test-owned path.
	if err != nil || !bytes.Contains(contents, []byte(`"generation": 2`)) {
		t.Fatalf("replacement manifest=%q error=%v", contents, err)
	}
}

func TestRuntimeReleaseCacheIncludesCompleteLaunchIdentity(t *testing.T) {
	base := installedFixture(t, t.TempDir(), "a").Release
	for name, mutate := range map[string]func(*central.RuntimeRelease){
		"client version":     func(release *central.RuntimeRelease) { release.Client.Version = "2.0.0" },
		"client protocol":    func(release *central.RuntimeRelease) { release.Client.Protocol++ },
		"browser revision":   func(release *central.RuntimeRelease) { release.Browser.Revision = "2" },
		"Playwright version": func(release *central.RuntimeRelease) { release.Playwright.Version = "2.0.0" },
		"Probe keyring": func(release *central.RuntimeRelease) {
			release.Client.ProbeBootstrapPublicKeys = map[string]string{"rotated": testPublicKeyPEM(t)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			if sameRuntimeRelease(base, changed) {
				t.Fatal("changed launch identity reused the installed runtime")
			}
		})
	}
}

func TestRuntimeRollbackRestoresPreviousManifest(t *testing.T) {
	dataDir := t.TempDir()
	previous := installedFixture(t, dataDir, "1")
	current := installedFixture(t, dataDir, "2")
	current.Previous = &previous
	runtimeRoot := filepath.Join(dataDir, "runtime")
	if err := writeJSONAtomic(filepath.Join(runtimeRoot, "installed.json"), current); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(runtimeRoot, "previous.json"), previous); err != nil {
		t.Fatal(err)
	}
	if err := rollbackInstalledRelease(dataDir, current); err != nil {
		t.Fatal(err)
	}
	restored, err := loadInstalledManifest(dataDir, filepath.Join(runtimeRoot, "installed.json"))
	if err != nil || !sameRuntimeRelease(restored.Release, previous.Release) {
		t.Fatalf("restored runtime = %+v, error = %v", restored.Release, err)
	}
	if _, err := os.Stat(filepath.Join(runtimeRoot, "previous.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previous slot was not consumed: %v", err)
	}
	if _, err := os.Stat(current.ClientPath); err != nil {
		t.Fatalf("failed runtime was deleted before rollback completed: %v", err)
	}
}

func installedFixture(t *testing.T, dataDir string, suffix string) installedRelease {
	t.Helper()
	digest := strings.Repeat(suffix, 64)
	paths := map[string]string{}
	for _, component := range []string{"client", "browser", "playwright"} {
		root := filepath.Join(dataDir, "components", component, digest)
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if component == "playwright" {
			if err := os.MkdirAll(filepath.Join(root, "package"), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{filepath.Join(root, "node"), filepath.Join(root, "package", "cli.js")} {
				writeExecutableFixture(t, path, []byte("fixture"))
			}
			paths[component] = root
		} else {
			path := filepath.Join(root, component)
			writeExecutableFixture(t, path, []byte("fixture"))
			paths[component] = path
		}
		if err := writeComponentIntegrity(root, digest); err != nil {
			t.Fatal(err)
		}
	}
	keyring := map[string]string{"primary": testPublicKeyPEM(t)}
	keyHash, keySpec, err := installProbePublicKeys(filepath.Join(dataDir, "components", "keyring"), keyring)
	if err != nil {
		t.Fatal(err)
	}
	return installedRelease{
		Release: central.RuntimeRelease{
			Client: central.ClientRelease{
				Artifact:                 central.ReleaseArtifact{SHA256: digest, Executable: "client"},
				ProbeBootstrapPublicKeys: keyring,
			},
			Browser: central.BrowserRelease{
				Artifact: central.ReleaseArtifact{SHA256: digest, Executable: "browser"},
			},
			Playwright: central.PlaywrightRelease{
				Artifact: central.ReleaseArtifact{SHA256: digest, Executable: "node"},
			},
		},
		ClientPath: paths["client"], BrowserPath: paths["browser"], DriverPath: paths["playwright"],
		ProbePublicKeyHash: keyHash, ProbePublicKeySpec: keySpec,
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

func TestArtifactDownloadRestartsWhenOriginIgnoresRange(t *testing.T) {
	payload := []byte("complete release payload")
	digest := sha256.Sum256(payload)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		_, _ = writer.Write(payload)
	}))
	t.Cleanup(server.Close)
	artifact := central.ReleaseArtifact{
		URL: server.URL + "/client.zip", Size: int64(len(payload)),
		SHA256: hex.EncodeToString(digest[:]), Executable: "client",
	}
	cache := t.TempDir()
	if err := os.WriteFile(filepath.Join(cache, artifact.SHA256+".blob.part"), payload[:4], 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := downloadArtifact(t.Context(), server.Client(), cache, "client", artifact, nil)
	if err != nil || !fileMatchesArtifact(path, artifact) {
		t.Fatalf("restart result path=%q error=%v", path, err)
	}
	if requests.Load() != 2 {
		t.Fatalf("origin fallback requests=%d", requests.Load())
	}
}

func TestArtifactDownloadDoesNotFollowPartialSymlink(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("symlink policy is host-dependent on Windows")
	}
	payload := []byte("verified release")
	digest := sha256.Sum256(payload)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(payload)
	}))
	t.Cleanup(server.Close)
	artifact := central.ReleaseArtifact{
		URL: server.URL + "/client.zip", Size: int64(len(payload)),
		SHA256: hex.EncodeToString(digest[:]), Executable: "client",
	}
	cache := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("do-not-change"), 0o600); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(cache, artifact.SHA256+".blob.part")
	if err := os.Symlink(outside, partial); err != nil {
		t.Fatal(err)
	}
	path, err := downloadArtifact(t.Context(), server.Client(), cache, "client", artifact, nil)
	if err != nil || !fileMatchesArtifact(path, artifact) {
		t.Fatalf("safe restart path=%q error=%v", path, err)
	}
	contents, err := os.ReadFile(outside) // #nosec G304 -- test-owned path.
	if err != nil || string(contents) != "do-not-change" {
		t.Fatalf("partial symlink target contents=%q error=%v", contents, err)
	}
}

func TestPortableLauncherDownloadVerifiesBeforeAtomicPlacement(t *testing.T) {
	payload := []byte("portable-launcher")
	digest := sha256.Sum256(payload)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(payload)
	}))
	t.Cleanup(server.Close)
	artifact := central.ReleaseArtifact{
		URL: server.URL + "/cineko-launcher.AppImage", Size: int64(len(payload)),
		SHA256: hex.EncodeToString(digest[:]), Executable: "cineko-launcher.AppImage",
	}
	destination := filepath.Join(t.TempDir(), "cineko-launcher.AppImage")
	if err := DownloadPortableLauncher(t.Context(), server.Client(), t.TempDir(), artifact, destination, nil); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(destination) // #nosec G304 -- test-owned destination.
	if err != nil || !bytes.Equal(contents, payload) {
		t.Fatalf("portable Launcher contents=%q error=%v", contents, err)
	}
	if os.PathSeparator == '/' {
		info, err := os.Stat(destination)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("AppImage mode=%v error=%v", info, err)
		}
	}
	artifact.SHA256 = strings.Repeat("0", 64)
	badDestination := destination + ".bad"
	if err := DownloadPortableLauncher(t.Context(), server.Client(), t.TempDir(), artifact, badDestination, nil); err == nil {
		t.Fatal("corrupt portable Launcher accepted")
	}
	if _, err := os.Stat(badDestination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unverified Launcher was placed: %v", err)
	}
}

func TestRuntimeCleanupRemovesOnlyUnreferencedComponentsAndArtifacts(t *testing.T) {
	dataDir := t.TempDir()
	active := central.RuntimeRelease{
		Client:     central.ClientRelease{Artifact: central.ReleaseArtifact{SHA256: strings.Repeat("a", 64)}},
		Browser:    central.BrowserRelease{Artifact: central.ReleaseArtifact{SHA256: strings.Repeat("b", 64)}},
		Playwright: central.PlaywrightRelease{Artifact: central.ReleaseArtifact{SHA256: strings.Repeat("c", 64)}},
	}
	for component, digest := range map[string]string{
		"client":     active.Client.Artifact.SHA256,
		"browser":    active.Browser.Artifact.SHA256,
		"playwright": active.Playwright.Artifact.SHA256,
	} {
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
	cleanupRuntime(dataDir, installedRelease{Release: active})
	for component, digest := range map[string]string{
		"client":     active.Client.Artifact.SHA256,
		"browser":    active.Browser.Artifact.SHA256,
		"playwright": active.Playwright.Artifact.SHA256,
	} {
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

func TestRuntimeCleanupDoesNotFollowManagedRootSymlink(t *testing.T) {
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
	cleanupRuntime(dataDir, installedRelease{Release: central.RuntimeRelease{
		Client:     central.ClientRelease{Artifact: central.ReleaseArtifact{SHA256: strings.Repeat("a", 64)}},
		Browser:    central.BrowserRelease{Artifact: central.ReleaseArtifact{SHA256: strings.Repeat("b", 64)}},
		Playwright: central.PlaywrightRelease{Artifact: central.ReleaseArtifact{SHA256: strings.Repeat("c", 64)}},
	}})
	if contents, err := os.ReadFile(protected); err != nil || string(contents) != "keep" { // #nosec G304 -- test-owned path.
		t.Fatalf("cleanup followed managed-root symlink: contents=%q error=%v", contents, err)
	}
}

func TestContentRangeValidationIsExact(t *testing.T) {
	for _, value := range []string{"bytes 4-9/10", " bytes 4-9/10 "} {
		if !validContentRange(value, 4, 10) {
			t.Fatalf("valid Content-Range rejected: %q", value)
		}
	}
	for _, value := range []string{
		"bytes 3-9/10", "bytes 4-8/10", "bytes 4-9/11", "items 4-9/10", "bytes garbage", "bytes 4-x/10",
	} {
		if validContentRange(value, 4, 10) {
			t.Fatalf("invalid Content-Range accepted: %q", value)
		}
	}
}
