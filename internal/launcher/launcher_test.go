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
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	clientpb "github.com/cineko-org/contracts/v3/gen/go/cineko/client"
	commonpb "github.com/cineko-org/contracts/v3/gen/go/cineko/common"
	releasepb "github.com/cineko-org/contracts/v3/gen/go/cineko/release"
	"github.com/cineko-org/launcher/internal/launcher/artifact"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestLauncherDownloadsVerifiesCachesAndRunsClient(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is covered on macOS and Linux runners")
	}
	relaunchMarker := filepath.Join(t.TempDir(), "client-requested-update")
	clientArchive := zipArtifact(t, "client.sh", fmt.Sprintf("#!/bin/sh\nif [ ! -e %q ]; then touch %q; exit 75; fi\npayload=$(cat)\n[ -z \"$CINEKO_CENTRAL_ACCESS_TOKEN\" ] || exit 21\n[ -x \"$CINEKO_CHROME_PATH\" ] || exit 22\n[ -x \"$CINEKO_PLAYWRIGHT_DRIVER_PATH\" ] || exit 23\nnonce=\"$CINEKO_STARTUP_READY_NONCE\"\n[ -n \"$nonce\" ] || exit 24\nprintf '%%s\\n' \"$nonce\" > \"$CINEKO_DATA_DIR/runtime/startup/$nonce.ready\"\nchmod 600 \"$CINEKO_DATA_DIR/runtime/startup/$nonce.ready\"\nprintf '%%s' \"$payload\"\n", relaunchMarker, relaunchMarker))
	browserArchive := zipArtifact(t, "browser", "#!/bin/sh\nexit 0\n")
	driverArchive := zipArtifacts(t, map[string]string{
		"node": "#!/bin/sh\nexit 0\n", "package/cli.js": "#!/usr/bin/env node\n",
	})
	var artifactRequests atomic.Int32
	var generation atomic.Int64
	generation.Store(17)
	var runtimeRequests atomic.Int32
	var ticketRequests atomic.Int32
	var release *releasepb.RuntimeRelease
	ticketRequest := &clientpb.LaunchTicketRequest{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/releases/runtime/current" && runtimeRequests.Add(1) == 3 {
			generation.Store(19)
		}
		if !strings.HasSuffix(request.URL.Path, ".zip") {
			writer.Header().Set("X-Cineko-Release-Generation", fmt.Sprint(generation.Load()))
		}
		switch request.URL.Path {
		case "/health":
			writeProtoJSON(t, writer, serviceHealthReady())
		case "/v1/auth/exchange":
			writeProtoJSON(t, writer, authenticationResponse(time.Now(), "session"))
		case "/v1/releases/runtime/current":
			writeProtoJSON(t, writer, release)
		case "/v1/releases/launcher/current":
			writeProtoJSON(t, writer, launcherRelease("1.0.0", testArtifact("https://cdn.example/launcher.zip", 1, strings.Repeat("a", 64), "launcher")))
		case "/v1/launch-tickets":
			if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(readBody(t, request), ticketRequest); err != nil {
				t.Errorf("decode launch ticket request: %v", err)
			}
			if request.Header.Get("Idempotency-Key") == "" {
				t.Error("launch ticket request is missing idempotency key")
			}
			if ticketRequests.Add(1) == 1 {
				generation.Store(18)
				writer.Header().Set("X-Cineko-Release-Generation", "18")
				writer.WriteHeader(http.StatusConflict)
				writeProtoJSON(t, writer, apiErrorResponse("stale_release", "runtime release changed"))
				return
			}
			ticket := &clientpb.LaunchTicketResponse{}
			ticket.SetLaunchTicket("launch-secret")
			ticket.SetExpiresAt(timestamppb.New(time.Now().Add(time.Minute)))
			writeProtoJSON(t, writer, ticket)
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
				device := &clientpb.Device{}
				if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(readBody(t, request), device); err != nil {
					t.Errorf("decode device request: %v", err)
				}
				writeProtoJSON(t, writer, device)
				return
			}
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	release = runtimeRelease(
		clientRelease("1.0.0", "1234", "1.61.1", releaseArtifact(server.URL+"/client.zip", clientArchive, "client.sh"), map[string]string{"primary": testPublicKeyPEM(t)}),
		browserRelease("1234", "1.61.1", releaseArtifact(server.URL+"/browser.zip", browserArchive, "browser")),
		playwrightRelease("1.61.1", releaseArtifact(server.URL+"/driver.zip", driverArchive, "node")),
	)
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
		!strings.Contains(output.String(), `"releaseGeneration":"19"`) ||
		!strings.Contains(output.String(), `"browserArtifactSha256":"`) ||
		!strings.Contains(output.String(), `"playwrightVersion":"1.61.1"`) ||
		!strings.Contains(output.String(), `"playwrightArtifactSha256":"`) {
		t.Fatalf("client launch payload = %q", output.String())
	}
	if ticketRequest.GetContext().GetReleaseGeneration() != 19 || ticketRequest.GetContext().GetClientVersion() != "1.0.0" ||
		ticketRequest.GetContext().GetBrowserArtifactSha256() != release.GetBrowser().GetArtifact().GetSha256() ||
		ticketRequest.GetContext().GetPlaywrightVersion() != "1.61.1" ||
		ticketRequest.GetContext().GetPlaywrightArtifactSha256() != release.GetPlaywright().GetArtifact().GetSha256() || ticketRequests.Load() != 3 {
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
		writer.Header().Set("X-Cineko-Release-Generation", "23")
		switch request.URL.Path {
		case "/health":
			writeProtoJSON(t, writer, serviceHealthReady())
		case "/v1/auth/exchange":
			writeProtoJSON(t, writer, authenticationResponse(time.Now(), "session"))
		case "/v1/releases/launcher/current":
			writeProtoJSON(t, writer, launcherRelease("1.1.0", testArtifact(
				"https://releases.example.com/cineko/launcher/v1.1.0/"+runtime.GOOS+"-"+runtime.GOARCH+"/cineko-launcher-v1.1.0.zip",
				1, strings.Repeat("a", 64), "Cineko Launcher",
			)))
		case "/v1/releases/runtime/current":
			runtimeRequests.Add(1)
			http.Error(writer, "must not load runtime", http.StatusInternalServerError)
		default:
			if strings.HasPrefix(request.URL.Path, "/v1/devices/") {
				device := &clientpb.Device{}
				if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(readBody(t, request), device); err != nil {
					t.Errorf("decode device request: %v", err)
				}
				writeProtoJSON(t, writer, device)
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
	if !errors.As(err, &update) || update.Version != "1.1.0" || !strings.HasPrefix(update.Artifact.GetUrl(), "https://releases.example.com/") {
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
	var release *releasepb.RuntimeRelease
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, ".zip") {
			writer.Header().Set("X-Cineko-Release-Generation", "31")
		}
		switch request.URL.Path {
		case "/health":
			writeProtoJSON(t, writer, serviceHealthReady())
		case "/v1/auth/exchange":
			writeProtoJSON(t, writer, authenticationResponse(time.Now(), "session"))
		case "/v1/releases/launcher/current":
			launcherChecks.Add(1)
			writeProtoJSON(t, writer, launcherRelease(launcherVersion, testArtifact("https://cdn.example/launcher.zip", 1, strings.Repeat("a", 64), "launcher")))
		case "/v1/releases/runtime/current":
			runtimeRequests.Add(1)
			writeProtoJSON(t, writer, release)
		case "/v1/launch-tickets":
			ticketRequests.Add(1)
			launcherVersion = "2.0.0"
			writer.WriteHeader(http.StatusConflict)
			writeProtoJSON(t, writer, apiErrorResponse("stale_release", "changed"))
		case "/client.zip":
			_, _ = writer.Write(clientArchive)
		case "/browser.zip":
			_, _ = writer.Write(browserArchive)
		case "/driver.zip":
			_, _ = writer.Write(driverArchive)
		default:
			if strings.HasPrefix(request.URL.Path, "/v1/devices/") {
				device := &clientpb.Device{}
				if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(readBody(t, request), device); err != nil {
					t.Errorf("decode device request: %v", err)
				}
				writeProtoJSON(t, writer, device)
				return
			}
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	release = runtimeRelease(
		clientRelease("1.0.0", "1", "1.0.0", releaseArtifact(server.URL+"/client.zip", clientArchive, "client"), map[string]string{"primary": testPublicKeyPEM(t)}),
		browserRelease("1", "1.0.0", releaseArtifact(server.URL+"/browser.zip", browserArchive, "browser")),
		playwrightRelease("1.0.0", releaseArtifact(server.URL+"/driver.zip", driverArchive, "node")),
	)
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
	validArtifact := testArtifact("https://releases.example.com/cineko/client/v1.0.0/darwin-arm64/client.zip", 1, strings.Repeat("a", 64), "bin/client")
	if err := artifact.ValidateMetadata(validArtifact); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*releasepb.Artifact){
		"insecure URL": func(value *releasepb.Artifact) { value.SetUrl("http://cdn.example/client.zip") },
		"empty size":   func(value *releasepb.Artifact) { value.SetSize(0) },
		"invalid hash": func(value *releasepb.Artifact) { value.SetSha256("invalid") },
		"escaping path": func(value *releasepb.Artifact) {
			value.SetExecutable("../client")
		},
	} {
		value := proto.CloneOf(validArtifact)
		mutate(value)
		if err := artifact.ValidateMetadata(value); err == nil {
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

func releaseArtifact(url string, contents []byte, executable string) *releasepb.Artifact {
	digest := sha256.Sum256(contents)
	return testArtifact(url, int64(len(contents)), hex.EncodeToString(digest[:]), executable)
}

func testArtifact(url string, size int64, sha256 string, executable string) *releasepb.Artifact {
	artifact := &releasepb.Artifact{}
	artifact.SetUrl(url)
	artifact.SetSize(size)
	artifact.SetSha256(sha256)
	artifact.SetExecutable(executable)
	return artifact
}

func authenticationResponse(now time.Time, accessToken string) *clientpb.AuthenticationResponse {
	auth := &clientpb.AuthenticationResponse{}
	auth.SetAccessToken(accessToken)
	auth.SetExpiresAt(timestamppb.New(now.Add(time.Hour)))
	auth.SetRefreshToken("refresh")
	auth.SetRefreshExpiresAt(timestamppb.New(now.Add(24 * time.Hour)))
	user := &clientpb.User{}
	user.SetId("user")
	auth.SetUser(user)
	return auth
}

func apiErrorResponse(code, message string) *commonpb.APIErrorResponse {
	response := &commonpb.APIErrorResponse{}
	errorValue := &commonpb.APIError{}
	errorValue.SetCode(code)
	errorValue.SetMessage(message)
	errorValue.SetRequestId("test-request")
	response.SetError(errorValue)
	return response
}

func serviceHealthReady() *commonpb.ServiceHealth {
	health := &commonpb.ServiceHealth{}
	health.SetReady(&commonpb.Ready{})
	return health
}

func launcherRelease(version string, artifact *releasepb.Artifact) *releasepb.LauncherRelease {
	release := &releasepb.LauncherRelease{}
	release.SetChannel("stable")
	release.SetPlatform(runtime.GOOS)
	release.SetArchitecture(runtime.GOARCH)
	release.SetVersion(version)
	release.SetLauncher(artifact)
	release.SetPublishedAt(timestamppb.New(time.Now().UTC()))
	return release
}

func runtimeRelease(client *releasepb.ClientRelease, browser *releasepb.BrowserRelease, playwright *releasepb.PlaywrightRelease) *releasepb.RuntimeRelease {
	release := &releasepb.RuntimeRelease{}
	release.SetClient(client)
	release.SetBrowser(browser)
	release.SetPlaywright(playwright)
	return release
}

func clientRelease(version, minimumBrowserRevision, playwrightVersion string, artifact *releasepb.Artifact, keyring map[string]string) *releasepb.ClientRelease {
	release := &releasepb.ClientRelease{}
	release.SetChannel("stable")
	release.SetPlatform(runtime.GOOS)
	release.SetArchitecture(runtime.GOARCH)
	release.SetVersion(version)
	release.SetMinimumLauncherVersion("1.0.0")
	release.SetMinimumBrowserRevision(minimumBrowserRevision)
	release.SetPlaywrightVersion(playwrightVersion)
	release.SetArtifact(artifact)
	release.SetProbeBootstrapPublicKeys(keyring)
	release.SetPublishedAt(timestamppb.New(time.Now().UTC()))
	return release
}

func browserRelease(revision, playwrightVersion string, artifact *releasepb.Artifact) *releasepb.BrowserRelease {
	release := &releasepb.BrowserRelease{}
	release.SetChannel("stable")
	release.SetPlatform(runtime.GOOS)
	release.SetArchitecture(runtime.GOARCH)
	release.SetRevision(revision)
	release.SetCompatiblePlaywrightVersions([]string{playwrightVersion})
	release.SetArtifact(artifact)
	release.SetPublishedAt(timestamppb.New(time.Now().UTC()))
	return release
}

func playwrightRelease(version string, artifact *releasepb.Artifact) *releasepb.PlaywrightRelease {
	release := &releasepb.PlaywrightRelease{}
	release.SetChannel("stable")
	release.SetPlatform(runtime.GOOS)
	release.SetArchitecture(runtime.GOARCH)
	release.SetVersion(version)
	release.SetArtifact(artifact)
	release.SetPublishedAt(timestamppb.New(time.Now().UTC()))
	return release
}

func writeProtoJSON(t *testing.T, writer http.ResponseWriter, message proto.Message) {
	t.Helper()
	contents, err := (protojson.MarshalOptions{UseProtoNames: false}).Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write(contents)
}

func readBody(t *testing.T, request *http.Request) []byte {
	t.Helper()
	contents, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}
