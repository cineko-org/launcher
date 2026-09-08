package launcher

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	clientpb "github.com/cineko-org/contracts/v3/gen/go/cineko/client"
	releasepb "github.com/cineko-org/contracts/v3/gen/go/cineko/release"
	"github.com/cineko-org/launcher/internal/launcher/artifact"
	"github.com/cineko-org/launcher/internal/launcher/managedfiles"
	"github.com/cineko-org/launcher/internal/telemetry"
	"github.com/cineko-org/probe/v2/networkcapture"

	"golang.org/x/mod/semver"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const maximumReleaseManifestBytes = 2 << 20

type Config struct {
	ReleaseBaseURL  string
	ClientPath      string
	ChromePath      string
	DriverPath      string
	DataDir         string
	Version         string
	Debug           bool
	HTTPClient      *http.Client
	Logger          *slog.Logger
	Stdout          io.Writer
	Stderr          io.Writer
	OnProgress      func(Progress)
	OnClientStarted func(pid int)
	OnClientStopped func()
	NetworkCapture  *networkcapture.Store
}

type LauncherUpdateRequired struct {
	Version  string
	Artifact *releasepb.Artifact
}

func (update *LauncherUpdateRequired) Error() string {
	return fmt.Sprintf("Launcher %s download is required", update.Version)
}

type Stage string

const (
	StageChecking    Stage = "checking"
	StageDownloading Stage = "downloading"
	StageInstalling  Stage = "installing"
	StageLaunching   Stage = "launching"
	StageRunning     Stage = "running"
)

type Progress struct {
	Stage      Stage  `json:"stage"`
	Message    string `json:"message"`
	Artifact   string `json:"artifact,omitempty"`
	Downloaded int64  `json:"downloaded,omitempty"`
	Total      int64  `json:"total,omitempty"`
}

type identity struct {
	InstallationID string `json:"installationId"`
	DeviceID       string `json:"deviceId"`
}

type installedRelease struct {
	Release     *releasepb.RuntimeRelease `json:"release"`
	ClientPath  string                    `json:"clientPath"`
	BrowserPath string                    `json:"browserPath"`
	DriverPath  string                    `json:"driverPath"`
	Previous    *installedRelease         `json:"-"`
}

// Run starts a completely local Client. A configured public release directory
// is used only to check and install updates; it is never an application server.
func Run(ctx context.Context, config Config) error {
	ctx = telemetry.WithLogger(ctx, config.Logger)
	if err := validateConfig(config); err != nil {
		return err
	}
	if config.NetworkCapture == nil {
		capture, err := networkcapture.NewStore(filepath.Join(config.DataDir, "artifacts", "network"), config.Logger, networkcapture.WithDebug(config.Debug))
		if err != nil {
			return fmt.Errorf("initialize Launcher network capture: %w", err)
		}
		config.NetworkCapture = capture
	}
	installation, err := loadOrCreateIdentity(config.DataDir)
	if err != nil {
		return err
	}
	if strings.TrimSpace(config.ClientPath) != "" {
		installed, err := directClient(config)
		if err != nil {
			return err
		}
		_, err = runClient(ctx, config, installed, installation, nil, nil)
		return err
	}

	installed, err := resolveRuntime(ctx, config)
	if err != nil {
		return err
	}
	rollback := func(cause error) error {
		if rollbackErr := rollbackInstalledRelease(config.DataDir, installed); rollbackErr != nil {
			return errors.Join(cause, fmt.Errorf("rollback Client runtime: %w", rollbackErr))
		}
		return cause
	}
	ready, err := runClient(ctx, config, installed, installation, func() {
		finalizeInstalledRelease(config.DataDir, installed)
	}, installed.Release)
	if err != nil && !ready {
		return rollback(err)
	}
	return err
}

func resolveRuntime(ctx context.Context, config Config) (installedRelease, error) {
	manifestPath := filepath.Join(config.DataDir, "runtime", "installed.json")
	if strings.TrimSpace(config.ReleaseBaseURL) == "" {
		report(config, Progress{Stage: StageChecking, Message: "설치된 Client 확인 중"})
		installed, err := loadInstalledManifest(config.DataDir, manifestPath)
		if err != nil {
			return installedRelease{}, errors.New("설치된 Client가 없습니다. CINEKO_CLIENT_PATH 또는 CINEKO_RELEASE_BASE_URL을 설정하세요")
		}
		return installed, nil
	}

	report(config, Progress{Stage: StageChecking, Message: "공개 릴리스 확인 중"})
	launcherRelease := &releasepb.LauncherRelease{}
	if err := fetchReleaseProto(ctx, config, "launcher.json", launcherRelease); err != nil {
		return fallbackInstalled(config, manifestPath, fmt.Errorf("load Launcher release: %w", err))
	}
	if err := validateLauncherRelease(launcherRelease); err != nil {
		return fallbackInstalled(config, manifestPath, err)
	}
	if semver.Compare(canonicalVersion(launcherRelease.GetVersion()), canonicalVersion(config.Version)) > 0 {
		return installedRelease{}, &LauncherUpdateRequired{Version: launcherRelease.GetVersion(), Artifact: launcherRelease.GetLauncher()}
	}
	runtimeRelease := &releasepb.RuntimeRelease{}
	if err := fetchReleaseProto(ctx, config, "runtime.json", runtimeRelease); err != nil {
		return fallbackInstalled(config, manifestPath, fmt.Errorf("load Client release: %w", err))
	}
	if err := validateReleaseForLauncher(runtimeRelease, config.Version); err != nil {
		return fallbackInstalled(config, manifestPath, err)
	}
	return installRelease(ctx, config, runtimeRelease)
}

func fallbackInstalled(config Config, manifestPath string, remoteErr error) (installedRelease, error) {
	if config.Logger != nil {
		config.Logger.Warn("public release check failed; using installed Client", "error", remoteErr)
	}
	installed, err := loadInstalledManifest(config.DataDir, manifestPath)
	if err != nil {
		return installedRelease{}, remoteErr
	}
	report(config, Progress{Stage: StageChecking, Message: "설치된 Client로 시작"})
	return installed, nil
}

func fetchReleaseProto(ctx context.Context, config Config, name string, destination proto.Message) error {
	ctx = telemetry.WithLogger(ctx, config.Logger)
	repository := "client"
	if name == "launcher.json" {
		repository = "launcher"
	} else if name != "runtime.json" {
		return fmt.Errorf("unsupported release manifest %q", name)
	}
	asset := strings.TrimSuffix(name, ".json") + "-" + runtime.GOOS + "-" + runtime.GOARCH + ".json"
	endpoint := strings.TrimRight(config.ReleaseBaseURL, "/") + "/" + repository + "/releases/latest/download/" + asset
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	telemetry.EnsureRequestID(request)
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	client = networkcapture.HTTPClient(config.NetworkCapture, "launcher", config.Logger, client)
	started := time.Now()
	response, err := client.Do(request)
	if err != nil {
		telemetry.LogHTTPClientRequest(ctx, request, nil, started, 0, 0, err)
		return err
	}
	defer func() { _ = response.Body.Close() }()
	contents, readErr := io.ReadAll(io.LimitReader(response.Body, maximumReleaseManifestBytes+1))
	telemetry.LogHTTPClientRequest(ctx, request, response, started, 0, int64(len(contents)), readErr)
	if readErr != nil {
		return readErr
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("release manifest returned HTTP %d", response.StatusCode)
	}
	if len(contents) == 0 || len(contents) > maximumReleaseManifestBytes {
		return errors.New("release manifest is empty or too large")
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(contents, destination); err != nil {
		return fmt.Errorf("decode release manifest: %w", err)
	}
	return nil
}

func directClient(config Config) (installedRelease, error) {
	clientPath := filepath.Clean(strings.TrimSpace(config.ClientPath))
	if !filepath.IsAbs(clientPath) {
		return installedRelease{}, errors.New("CINEKO_CLIENT_PATH must be absolute")
	}
	info, err := os.Stat(clientPath)
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return installedRelease{}, errors.New("CINEKO_CLIENT_PATH must point to an executable Client")
	}
	return installedRelease{
		ClientPath:  clientPath,
		BrowserPath: strings.TrimSpace(config.ChromePath),
		DriverPath:  strings.TrimSpace(config.DriverPath),
	}, nil
}

func validateConfig(config Config) error {
	if strings.TrimSpace(config.DataDir) == "" {
		return errors.New("launcher data directory is required")
	}
	if strings.TrimSpace(config.ReleaseBaseURL) != "" {
		if err := validateReleaseBaseURL(config.ReleaseBaseURL); err != nil {
			return err
		}
	}
	if !semver.IsValid(canonicalVersion(config.Version)) {
		return errors.New("launcher version must be semantic versioning")
	}
	return nil
}

func validateReleaseBaseURL(rawURL string) error {
	endpoint, err := url.ParseRequestURI(strings.TrimSpace(rawURL))
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return errors.New("release base URL must be an origin or directory without credentials, query, or fragment")
	}
	if endpoint.Scheme == "https" {
		return nil
	}
	hostname := strings.ToLower(endpoint.Hostname())
	address := net.ParseIP(hostname)
	if endpoint.Scheme == "http" && (hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") || address != nil && address.IsLoopback()) {
		return nil
	}
	return errors.New("release base URL must use HTTPS unless it targets loopback")
}

func validateLauncherRelease(release *releasepb.LauncherRelease) error {
	if release == nil || release.GetChannel() != "stable" || release.GetPlatform() != runtime.GOOS ||
		release.GetArchitecture() != runtime.GOARCH || !semver.IsValid(canonicalVersion(release.GetVersion())) {
		return errors.New("release is incompatible with this launcher")
	}
	if err := artifact.ValidateMetadata(release.GetLauncher()); err != nil {
		return fmt.Errorf("launcher download: %w", err)
	}
	return nil
}

//nolint:gocyclo,cyclop // Runtime compatibility keeps every release component invariant explicit.
func validateReleaseForLauncher(release *releasepb.RuntimeRelease, launcherVersion string) error {
	if release == nil {
		return errors.New("release is incompatible with this launcher")
	}
	client := release.GetClient()
	browser := release.GetBrowser()
	playwright := release.GetPlaywright()
	if client == nil || browser == nil || playwright == nil ||
		client.GetChannel() != "stable" || browser.GetChannel() != "stable" || playwright.GetChannel() != "stable" ||
		client.GetPlatform() != runtime.GOOS || client.GetArchitecture() != runtime.GOARCH ||
		browser.GetPlatform() != runtime.GOOS || browser.GetArchitecture() != runtime.GOARCH ||
		playwright.GetPlatform() != runtime.GOOS || playwright.GetArchitecture() != runtime.GOARCH {
		return errors.New("release is incompatible with this launcher")
	}
	if err := validateRuntimeCompatibility(release, launcherVersion); err != nil {
		return err
	}
	for _, component := range []struct {
		name     string
		artifact *releasepb.Artifact
	}{{"client", client.GetArtifact()}, {"browser", browser.GetArtifact()}, {"playwright", playwright.GetArtifact()}} {
		if err := artifact.ValidateMetadata(component.artifact); err != nil {
			return fmt.Errorf("%s artifact: %w", component.name, err)
		}
	}
	return nil
}

func validateRuntimeCompatibility(release *releasepb.RuntimeRelease, launcherVersion string) error {
	client := release.GetClient()
	minimum := canonicalVersion(client.GetMinimumLauncherVersion())
	if !semver.IsValid(minimum) || semver.Compare(canonicalVersion(launcherVersion), minimum) < 0 {
		return fmt.Errorf("launcher %s is older than required %s", launcherVersion, client.GetMinimumLauncherVersion())
	}
	if !semver.IsValid(canonicalVersion(client.GetVersion())) ||
		!semver.IsValid(canonicalVersion(release.GetPlaywright().GetVersion())) ||
		!validNumericRevision(release.GetBrowser().GetRevision()) ||
		!validNumericRevision(client.GetMinimumBrowserRevision()) ||
		client.GetPlaywrightVersion() != release.GetPlaywright().GetVersion() ||
		compareNumericRevision(release.GetBrowser().GetRevision(), client.GetMinimumBrowserRevision()) < 0 ||
		!containsString(release.GetBrowser().GetCompatiblePlaywrightVersions(), release.GetPlaywright().GetVersion()) {
		return errors.New("release client version is invalid")
	}
	return nil
}

func runClient(
	ctx context.Context,
	config Config,
	installed installedRelease,
	installation identity,
	onReady func(),
	release *releasepb.RuntimeRelease,
) (bool, error) {
	startupNonce, err := randomToken(24)
	if err != nil {
		return false, fmt.Errorf("create Client startup handshake: %w", err)
	}
	startupMarker, err := prepareStartupReady(config.DataDir, startupNonce)
	if err != nil {
		return false, fmt.Errorf("prepare Client startup handshake: %w", err)
	}
	defer func() { _ = os.Remove(startupMarker) }()
	clientVersion := "dev"
	launchContext := clientpb.LaunchContext_builder{
		InstallationId: &installation.InstallationID,
		DeviceId:       &installation.DeviceID,
		ClientVersion:  &clientVersion,
	}.Build()
	if release != nil {
		launchContext.SetClientVersion(release.GetClient().GetVersion())
	}
	envelope := clientpb.LaunchEnvelope_builder{Context: launchContext}.Build()
	payload, err := protojson.MarshalOptions{UseProtoNames: false}.Marshal(envelope)
	if err != nil {
		return false, fmt.Errorf("encode Client launch payload: %w", err)
	}
	processContext, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(processContext, installed.ClientPath) // #nosec G204 -- explicit local or hash-verified Client path.
	command.Stdin = strings.NewReader(string(payload))
	command.Stdout = defaultWriter(config.Stdout, os.Stdout)
	command.Stderr = defaultWriter(config.Stderr, os.Stderr)
	command.Env = append(sanitizedEnvironment(os.Environ()),
		"CINEKO_DATA_DIR="+config.DataDir,
		"CINEKO_STARTUP_READY_NONCE="+startupNonce,
		"CINEKO_CHROME_PATH="+installed.BrowserPath,
		"CINEKO_PLAYWRIGHT_DRIVER_PATH="+installed.DriverPath,
	)
	report(config, Progress{Stage: StageLaunching, Message: "Cineko Client 시작 중"})
	if err := command.Start(); err != nil {
		return false, fmt.Errorf("run Cineko Client: %w", err)
	}
	processDone := make(chan error, 1)
	go func() { processDone <- command.Wait() }()
	if err := awaitStartupReady(ctx, startupMarker, startupNonce, processDone, clientStartupTimeout, startupCheckInterval); err != nil {
		cancel()
		_ = command.Process.Kill()
		select {
		case <-processDone:
		case <-time.After(time.Second):
		}
		return false, fmt.Errorf("start Cineko Client: %w", err)
	}
	if onReady != nil {
		onReady()
	}
	report(config, Progress{Stage: StageRunning, Message: "Cineko Client 실행 중"})
	if config.OnClientStarted != nil {
		config.OnClientStarted(command.Process.Pid)
	}
	if config.OnClientStopped != nil {
		defer config.OnClientStopped()
	}
	if err := <-processDone; err != nil {
		return true, fmt.Errorf("wait for Cineko Client: %w", err)
	}
	return true, nil
}

func validNumericRevision(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func compareNumericRevision(left, right string) int {
	left = strings.TrimLeft(strings.TrimSpace(left), "0")
	right = strings.TrimLeft(strings.TrimSpace(right), "0")
	if len(left) != len(right) {
		if len(left) < len(right) {
			return -1
		}
		return 1
	}
	return strings.Compare(left, right)
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func report(config Config, progress Progress) {
	if config.OnProgress != nil {
		config.OnProgress(progress)
	}
}

func sanitizedEnvironment(environment []string) []string {
	blocked := map[string]struct{}{
		"CINEKO_DATA_DIR":   {},
		"CINEKO_DEV_DIRECT": {},
	}
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if _, remove := blocked[name]; !remove {
			result = append(result, entry)
		}
	}
	return result
}

func canonicalVersion(value string) string {
	value = strings.TrimSpace(value)
	if value != "" && value[0] != 'v' {
		return "v" + value
	}
	return value
}

func randomToken(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate launcher nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func defaultWriter(value io.Writer, fallback io.Writer) io.Writer {
	if value != nil {
		return value
	}
	return fallback
}

func loadOrCreateIdentity(dataDir string) (identity, error) {
	value, err := loadIdentity(dataDir)
	if err == nil {
		return value, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return identity{}, err
	}
	installation, err := randomToken(16)
	if err != nil {
		return identity{}, err
	}
	device, err := randomToken(16)
	if err != nil {
		return identity{}, err
	}
	value = identity{InstallationID: "install_" + installation, DeviceID: "device_" + device}
	if err := managedfiles.WriteJSONAtomic(filepath.Join(dataDir, "installation.json"), value); err != nil {
		return identity{}, err
	}
	return value, nil
}

func loadIdentity(dataDir string) (identity, error) {
	contents, err := os.ReadFile(filepath.Join(dataDir, "installation.json")) // #nosec G304 -- scoped launcher state path.
	if err != nil {
		return identity{}, fmt.Errorf("read launcher installation identity: %w", err)
	}
	var value identity
	if json.Unmarshal(contents, &value) != nil || value.InstallationID == "" || value.DeviceID == "" {
		return identity{}, errors.New("launcher installation identity is invalid")
	}
	return value, nil
}
