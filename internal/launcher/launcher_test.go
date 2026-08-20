package launcher

import (
	"archive/zip"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	central "github.com/cineko-org/contracts/v3"
)

func TestLauncherDownloadsVerifiesCachesAndRunsClient(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is covered on macOS and Linux runners")
	}
	relaunchMarker := filepath.Join(t.TempDir(), "client-requested-update")
	clientArchive := zipArtifact(t, "client.sh", fmt.Sprintf("#!/bin/sh\nif [ ! -e %q ]; then touch %q; exit 75; fi\npayload=$(cat)\n[ -z \"$CINEKO_CENTRAL_ACCESS_TOKEN\" ] || exit 21\n[ -x \"$CINEKO_CHROME_PATH\" ] || exit 22\n[ -x \"$CINEKO_PLAYWRIGHT_DRIVER_PATH\" ] || exit 23\nnonce=$(printf '%%s' \"$payload\" | sed -n 's/.*\"startupReadyNonce\":\"\\([^\"]*\\)\".*/\\1/p')\n[ -n \"$nonce\" ] || exit 24\nprintf '%%s\\n' \"$nonce\" > \"$CINEKO_DATA_DIR/runtime/startup/$nonce.ready\"\nchmod 600 \"$CINEKO_DATA_DIR/runtime/startup/$nonce.ready\"\nprintf '%%s' \"$payload\"\n", relaunchMarker, relaunchMarker))
	browserArchive := zipArtifact(t, "browser", "#!/bin/sh\nexit 0\n")
	driverArchive := zipArtifacts(t, map[string]string{
		"node": "#!/bin/sh\nexit 0\n", "package/cli.js": "#!/usr/bin/env node\n",
	})
	var artifactRequests atomic.Int32
	var generation atomic.Int64
	generation.Store(17)
	var runtimeRequests atomic.Int32
	var ticketRequests atomic.Int32
	var release central.RuntimeRelease
	var ticketRequest central.LaunchTicketRequest
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/releases/runtime/current" && runtimeRequests.Add(1) == 3 {
			generation.Store(19)
		}
		if !strings.HasSuffix(request.URL.Path, ".zip") {
			writer.Header().Set(central.ReleaseGenerationHeader, fmt.Sprint(generation.Load()))
		}
		switch request.URL.Path {
		case "/health":
			_ = json.NewEncoder(writer).Encode(map[string]string{"status": "ready"})
		case "/v1/auth/exchange":
			_ = json.NewEncoder(writer).Encode(central.AuthExchangeResponse{ // #nosec G117 -- test-only session fixture.
				AccessToken: "session", ExpiresAt: time.Now().Add(time.Hour),
				RefreshToken: "refresh", RefreshExpiresAt: time.Now().Add(24 * time.Hour),
				User: central.ClientUser{ID: "user"},
			})
		case "/v1/releases/runtime/current":
			_ = json.NewEncoder(writer).Encode(release)
		case "/v1/releases/launcher/current":
			_ = json.NewEncoder(writer).Encode(central.LauncherRelease{
				Channel: "stable", Platform: runtime.GOOS, Arch: runtime.GOARCH,
				Version: "1.0.0", Protocol: central.ProtocolVersion,
				Launcher: central.ReleaseArtifact{
					URL: "https://cdn.example/launcher.zip", Size: 1,
					SHA256: strings.Repeat("a", 64), Executable: "launcher",
				},
				PublishedAt: time.Now().UTC(),
			})
		case "/v1/launch-tickets":
			if err := json.NewDecoder(request.Body).Decode(&ticketRequest); err != nil {
				t.Errorf("decode launch ticket request: %v", err)
			}
			if request.Header.Get("Idempotency-Key") == "" {
				t.Error("launch ticket request is missing idempotency key")
			}
			if ticketRequests.Add(1) == 1 {
				generation.Store(18)
				writer.Header().Set(central.ReleaseGenerationHeader, "18")
				writer.WriteHeader(http.StatusConflict)
				_, _ = writer.Write([]byte(`{"error":{"code":"stale_release","message":"runtime release changed"}}`))
				return
			}
			_ = json.NewEncoder(writer).Encode(central.LaunchTicketResponse{
				LaunchTicket: "launch-secret", ExpiresAt: time.Now().Add(time.Minute),
			})
		case "/client.zip":
			if request.Header.Get("Authorization") != "" {
				t.Error("public client artifact request contains Central authorization")
			}
			artifactRequests.Add(1)
			_, _ = writer.Write(clientArchive)
		case "/browser.zip":
			if request.Header.Get("Authorization") != "" {
				t.Error("public browser artifact request contains Central authorization")
			}
			artifactRequests.Add(1)
			_, _ = writer.Write(browserArchive)
		case "/driver.zip":
			if request.Header.Get("Authorization") != "" {
				t.Error("public Playwright artifact request contains Central authorization")
			}
			artifactRequests.Add(1)
			_, _ = writer.Write(driverArchive)
		default:
			if strings.HasPrefix(request.URL.Path, "/v1/devices/") {
				var device central.ClientDevice
				_ = json.NewDecoder(request.Body).Decode(&device)
				_ = json.NewEncoder(writer).Encode(device)
				return
			}
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	release = central.RuntimeRelease{
		Client: central.ClientRelease{
			Channel: "stable", Platform: runtime.GOOS, Arch: runtime.GOARCH, Version: "1.0.0",
			MinimumLauncherVersion: "1.0.0", MinimumBrowserRevision: "1234",
			PlaywrightVersion: "1.61.1", Protocol: central.ProtocolVersion,
			Artifact:                 artifact(server.URL+"/client.zip", clientArchive, "client.sh"),
			ProbeBootstrapPublicKeys: map[string]string{"primary": testPublicKeyPEM(t)},
			PublishedAt:              time.Now().UTC(),
		},
		Browser: central.BrowserRelease{
			Channel: "stable", Platform: runtime.GOOS, Arch: runtime.GOARCH, Revision: "1234",
			CompatiblePlaywrightVersions: []string{"1.61.1"},
			Artifact:                     artifact(server.URL+"/browser.zip", browserArchive, "browser"), PublishedAt: time.Now().UTC(),
		},
		Playwright: central.PlaywrightRelease{
			Channel: "stable", Platform: runtime.GOOS, Arch: runtime.GOARCH, Version: "1.61.1",
			Artifact: artifact(server.URL+"/driver.zip", driverArchive, "node"), PublishedAt: time.Now().UTC(),
		},
	}
	dataDir := t.TempDir()
	var output bytes.Buffer
	var progress []Progress
	clientStarts := 0
	config := Config{
		CentralURL: server.URL, UserID: "user", AccessToken: "credential", DataDir: dataDir,
		Version: "1.0.0", HTTPClient: server.Client(), Stdout: &output, Stderr: &output,
		OnProgress:      func(value Progress) { progress = append(progress, value) },
		OnClientStarted: func() { clientStarts++ },
	}
	if err := Run(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"launchTicket":"launch-secret"`) ||
		!strings.Contains(output.String(), `"installationId":"install_`) ||
		!strings.Contains(output.String(), `"releaseGeneration":19`) ||
		!strings.Contains(output.String(), `"browserArtifactSha256":"`) ||
		!strings.Contains(output.String(), `"playwrightVersion":"1.61.1"`) ||
		!strings.Contains(output.String(), `"playwrightArtifactSha256":"`) {
		t.Fatalf("client launch payload = %q", output.String())
	}
	if ticketRequest.ReleaseGeneration != 19 || ticketRequest.ClientVersion != "1.0.0" ||
		ticketRequest.BrowserArtifactSHA256 != release.Browser.Artifact.SHA256 ||
		ticketRequest.PlaywrightVersion != "1.61.1" ||
		ticketRequest.PlaywrightArtifactSHA256 != release.Playwright.Artifact.SHA256 || ticketRequests.Load() != 3 {
		t.Fatalf("launch ticket request = %+v", ticketRequest)
	}
	if artifactRequests.Load() != 3 {
		t.Fatalf("artifact requests = %d", artifactRequests.Load())
	}
	if clientStarts != 1 || runtimeRequests.Load() != 3 || !containsStage(progress, StageDownloading) || !containsStage(progress, StageRunning) {
		t.Fatalf("first launch progress = %+v, client starts = %d", progress, clientStarts)
	}
	output.Reset()
	progress = nil
	if err := Run(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	if artifactRequests.Load() != 3 {
		t.Fatalf("cached launch artifact requests = %d", artifactRequests.Load())
	}
	if clientStarts != 2 || containsStage(progress, StageDownloading) || !containsStage(progress, StageInstalling) {
		t.Fatalf("cached launch progress = %+v, client starts = %d", progress, clientStarts)
	}
	identityInfo, err := os.Stat(filepath.Join(dataDir, "installation.json"))
	if err != nil || identityInfo.Mode().Perm() != 0o600 {
		t.Fatalf("identity permissions = %v, %v", identityInfo, err)
	}
}

func TestLauncherRequiresManualPortableUpdateBeforeRuntimeDownload(t *testing.T) {
	var runtimeRequests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set(central.ReleaseGenerationHeader, "23")
		switch request.URL.Path {
		case "/health":
			_ = json.NewEncoder(writer).Encode(map[string]string{"status": "ready"})
		case "/v1/auth/exchange":
			_ = json.NewEncoder(writer).Encode(central.AuthExchangeResponse{ // #nosec G117 -- test-only fixture.
				AccessToken: "session", ExpiresAt: time.Now().Add(time.Hour),
				RefreshToken: "refresh", RefreshExpiresAt: time.Now().Add(24 * time.Hour),
				User: central.ClientUser{ID: "user"},
			})
		case "/v1/releases/launcher/current":
			_ = json.NewEncoder(writer).Encode(central.LauncherRelease{
				Channel: "stable", Platform: runtime.GOOS, Arch: runtime.GOARCH,
				Version: "1.1.0", Protocol: central.ProtocolVersion,
				Launcher: central.ReleaseArtifact{
					URL:  "https://releases.example.com/cineko/launcher/v1.1.0/" + runtime.GOOS + "-" + runtime.GOARCH + "/cineko-launcher-v1.1.0.zip",
					Size: 1, SHA256: strings.Repeat("a", 64), Executable: "Cineko Launcher",
				},
				PublishedAt: time.Now().UTC(),
			})
		case "/v1/releases/runtime/current":
			runtimeRequests.Add(1)
			http.Error(writer, "must not load runtime", http.StatusInternalServerError)
		default:
			if strings.HasPrefix(request.URL.Path, "/v1/devices/") {
				var device central.ClientDevice
				_ = json.NewDecoder(request.Body).Decode(&device)
				_ = json.NewEncoder(writer).Encode(device)
				return
			}
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	err := Run(t.Context(), Config{
		CentralURL: server.URL, UserID: "user", AccessToken: "credential", DataDir: t.TempDir(),
		Version: "1.0.0", HTTPClient: server.Client(),
	})
	var update *LauncherUpdateRequired
	if !errors.As(err, &update) || update.Version != "1.1.0" || !strings.HasPrefix(update.Artifact.URL, "https://releases.example.com/") {
		t.Fatalf("portable Launcher update error = %v", err)
	}
	if runtimeRequests.Load() != 0 {
		t.Fatalf("runtime requests before Launcher update = %d", runtimeRequests.Load())
	}
}

func TestLauncherPublishedDuringRuntimeRetryBlocksBeforeRetry(t *testing.T) {
	clientArchive := zipArtifact(t, "client", "client")
	browserArchive := zipArtifact(t, "browser", "browser")
	driverArchive := zipArtifacts(t, map[string]string{"node": "node", "package/cli.js": "cli"})
	var launcherChecks atomic.Int32
	var runtimeRequests atomic.Int32
	var ticketRequests atomic.Int32
	launcherVersion := "1.0.0"
	var release central.RuntimeRelease
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, ".zip") {
			writer.Header().Set(central.ReleaseGenerationHeader, "31")
		}
		switch request.URL.Path {
		case "/health":
			_ = json.NewEncoder(writer).Encode(map[string]string{"status": "ready"})
		case "/v1/auth/exchange":
			_ = json.NewEncoder(writer).Encode(central.AuthExchangeResponse{ // #nosec G117 -- test fixture.
				AccessToken: "session", ExpiresAt: time.Now().Add(time.Hour),
				RefreshToken: "refresh", RefreshExpiresAt: time.Now().Add(24 * time.Hour),
				User: central.ClientUser{ID: "user"},
			})
		case "/v1/releases/launcher/current":
			launcherChecks.Add(1)
			_ = json.NewEncoder(writer).Encode(central.LauncherRelease{
				Channel: "stable", Platform: runtime.GOOS, Arch: runtime.GOARCH,
				Version: launcherVersion, Protocol: central.ProtocolVersion,
				Launcher:    central.ReleaseArtifact{URL: "https://cdn.example/launcher.zip", Size: 1, SHA256: strings.Repeat("a", 64), Executable: "launcher"},
				PublishedAt: time.Now().UTC(),
			})
		case "/v1/releases/runtime/current":
			runtimeRequests.Add(1)
			_ = json.NewEncoder(writer).Encode(release)
		case "/v1/launch-tickets":
			ticketRequests.Add(1)
			launcherVersion = "2.0.0"
			writer.WriteHeader(http.StatusConflict)
			_, _ = writer.Write([]byte(`{"error":{"code":"stale_release","message":"changed"}}`))
		case "/client.zip":
			_, _ = writer.Write(clientArchive)
		case "/browser.zip":
			_, _ = writer.Write(browserArchive)
		case "/driver.zip":
			_, _ = writer.Write(driverArchive)
		default:
			if strings.HasPrefix(request.URL.Path, "/v1/devices/") {
				var device central.ClientDevice
				_ = json.NewDecoder(request.Body).Decode(&device)
				_ = json.NewEncoder(writer).Encode(device)
				return
			}
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	release = central.RuntimeRelease{
		Client: central.ClientRelease{
			Channel: "stable", Platform: runtime.GOOS, Arch: runtime.GOARCH, Version: "1.0.0",
			MinimumLauncherVersion: "1.0.0", MinimumBrowserRevision: "1", PlaywrightVersion: "1.0.0", Protocol: central.ProtocolVersion,
			Artifact:                 artifact(server.URL+"/client.zip", clientArchive, "client"),
			ProbeBootstrapPublicKeys: map[string]string{"primary": testPublicKeyPEM(t)}, PublishedAt: time.Now().UTC(),
		},
		Browser: central.BrowserRelease{
			Channel: "stable", Platform: runtime.GOOS, Arch: runtime.GOARCH, Revision: "1",
			CompatiblePlaywrightVersions: []string{"1.0.0"}, Artifact: artifact(server.URL+"/browser.zip", browserArchive, "browser"), PublishedAt: time.Now().UTC(),
		},
		Playwright: central.PlaywrightRelease{
			Channel: "stable", Platform: runtime.GOOS, Arch: runtime.GOARCH, Version: "1.0.0",
			Artifact: artifact(server.URL+"/driver.zip", driverArchive, "node"), PublishedAt: time.Now().UTC(),
		},
	}
	err := Run(t.Context(), Config{
		CentralURL: server.URL, UserID: "user", AccessToken: "credential", DataDir: t.TempDir(), Version: "1.0.0", HTTPClient: server.Client(),
	})
	var update *LauncherUpdateRequired
	if !errors.As(err, &update) || update.Version != "2.0.0" {
		t.Fatalf("retry did not stop for newly published Launcher: %v", err)
	}
	if launcherChecks.Load() != 2 || runtimeRequests.Load() != 1 || ticketRequests.Load() != 1 {
		t.Fatalf("checks=%d runtime=%d tickets=%d", launcherChecks.Load(), runtimeRequests.Load(), ticketRequests.Load())
	}
}

func containsStage(progress []Progress, stage Stage) bool {
	for _, value := range progress {
		if value.Stage == stage {
			return true
		}
	}
	return false
}

func testPublicKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func TestArchiveExtractionRejectsEscapesAndLinks(t *testing.T) {
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

func TestLauncherValidation(t *testing.T) {
	if err := validateConfig(Config{}); err == nil {
		t.Fatal("empty launcher config accepted")
	}
	if err := validateConfig(Config{
		CentralURL: "https://central.example", UserID: "user", AccessToken: "token",
		DataDir: t.TempDir(), Version: "not-semver",
	}); err == nil {
		t.Fatal("invalid launcher version accepted")
	}
	for _, endpoint := range []string{
		"http://central.example",
		"https://user:password@central.example",
		"https://central.example/api",
		"https://central.example?source=launcher",
	} {
		if err := validateCentralURL(endpoint); err == nil {
			t.Fatalf("invalid Central URL %q accepted", endpoint)
		}
	}
	for _, endpoint := range []string{
		"https://central.example",
		"https://central.example/",
		"http://localhost:8080",
		"http://127.0.0.1:8080",
		"http://[::1]:8080",
	} {
		if err := validateCentralURL(endpoint); err != nil {
			t.Fatalf("valid Central URL %q rejected: %v", endpoint, err)
		}
	}
	if canonicalVersion(" 1.2.3 ") != "v1.2.3" || canonicalVersion("v1.2.3") != "v1.2.3" {
		t.Fatal("canonical version mismatch")
	}
	if got := sanitizedEnvironment([]string{
		"PATH=/bin", "CINEKO_CENTRAL_ACCESS_TOKEN=secret", "CINEKO_CENTRAL_USER_ID=user", "CINEKO_DEV_DIRECT=1",
	}); len(got) != 1 || got[0] != "PATH=/bin" {
		t.Fatalf("sanitized environment = %v", got)
	}
	validArtifact := central.ReleaseArtifact{
		URL:  "https://releases.example.com/cineko/client/v1.0.0/darwin-arm64/client.zip",
		Size: 1, SHA256: strings.Repeat("a", 64), Executable: "bin/client",
	}
	if err := validateArtifactMetadata(validArtifact); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*central.ReleaseArtifact){
		"insecure URL": func(value *central.ReleaseArtifact) { value.URL = "http://cdn.example/client.zip" },
		"empty size":   func(value *central.ReleaseArtifact) { value.Size = 0 },
		"invalid hash": func(value *central.ReleaseArtifact) { value.SHA256 = "invalid" },
		"escaping path": func(value *central.ReleaseArtifact) {
			value.Executable = "../client"
		},
	} {
		value := validArtifact
		mutate(&value)
		if err := validateArtifactMetadata(value); err == nil {
			t.Fatalf("%s artifact accepted", name)
		}
	}
}

func zipArtifact(t *testing.T, name string, contents string) []byte {
	return zipArtifacts(t, map[string]string{name: contents})
}

func zipArtifacts(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for name, contents := range files {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0o755)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func artifact(url string, contents []byte, executable string) central.ReleaseArtifact {
	digest := sha256.Sum256(contents)
	return central.ReleaseArtifact{
		URL: url, Size: int64(len(contents)), SHA256: hex.EncodeToString(digest[:]), Executable: executable,
	}
}
