package launcher

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	central "github.com/cineko-org/contracts/v3"
	centralstore "github.com/cineko-org/launcher/internal/centralclient"

	"golang.org/x/mod/semver"
)

type Config struct {
	CentralURL      string
	UserID          string
	AccessToken     string
	PIN             string
	DataDir         string
	Version         string
	HTTPClient      *http.Client
	Stdout          io.Writer
	Stderr          io.Writer
	OnProgress      func(Progress)
	OnClientStarted func()
}

type LauncherUpdateRequired struct {
	Version  string
	Artifact central.ReleaseArtifact
}

func (update *LauncherUpdateRequired) Error() string {
	return fmt.Sprintf("Launcher %s download is required", update.Version)
}

type Stage string

const (
	maximumReleaseAttempts       = 3
	clientUpdateRequiredExitCode = 75
)

const (
	StageAuthenticating Stage = "authenticating"
	StageChecking       Stage = "checking"
	StageDownloading    Stage = "downloading"
	StageInstalling     Stage = "installing"
	StageLaunching      Stage = "launching"
	StageRunning        Stage = "running"
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
	Release            central.RuntimeRelease `json:"release"`
	ClientPath         string                 `json:"clientPath"`
	BrowserPath        string                 `json:"browserPath"`
	DriverPath         string                 `json:"driverPath"`
	ProbePublicKeyHash string                 `json:"probePublicKeyHash"`
	ProbePublicKeySpec string                 `json:"probePublicKeySpec"`
	Previous           *installedRelease      `json:"-"`
}

func Run(ctx context.Context, config Config) error {
	if err := validateConfig(config); err != nil {
		return err
	}
	identity, store, err := prepareLauncher(ctx, config)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	for attempt := 0; attempt < maximumReleaseAttempts; attempt++ {
		if err := ensureLauncherCurrent(ctx, config, store); err != nil {
			return err
		}
		err = launchCurrentRuntime(ctx, config, identity, store)
		if !errors.Is(err, centralstore.ErrReleaseChanged) {
			return err
		}
	}
	return errors.New("runtime release changed repeatedly while preparing Client")
}

func prepareLauncher(
	ctx context.Context,
	config Config,
) (identity, *centralstore.Store, error) {
	report(config, Progress{Stage: StageChecking, Message: "Cineko 서버 연결 확인 중"})
	if err := centralstore.CheckHealth(ctx, config.CentralURL, config.HTTPClient); err != nil {
		return identity{}, nil, fmt.Errorf("check Central health: %w", err)
	}
	identity, err := loadOrCreateIdentity(config.DataDir)
	if err != nil {
		return identity, nil, err
	}
	report(config, Progress{Stage: StageAuthenticating, Message: "Central 로그인 확인 중"})
	store, err := authenticateLauncher(ctx, config, identity)
	if err != nil {
		return identity, nil, err
	}
	if _, err := store.RegisterDevice(ctx, central.ClientDevice{
		InstallationID: identity.InstallationID, DeviceID: identity.DeviceID,
		Platform: runtime.GOOS, Arch: runtime.GOARCH, AppVersion: "launcher/" + config.Version,
	}); err != nil {
		_ = store.Close()
		return identity, nil, fmt.Errorf("register launcher device: %w", err)
	}
	if err := saveLauncherSession(config.DataDir, store.Session()); err != nil {
		_ = store.Close()
		return identity, nil, fmt.Errorf("persist launcher session: %w", err)
	}
	return identity, store, nil
}

func ensureLauncherCurrent(ctx context.Context, config Config, store *centralstore.Store) error {
	launcherRelease, err := store.CurrentLauncherRelease(ctx, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("load current launcher release: %w", err)
	}
	if err := validateLauncherRelease(launcherRelease); err != nil {
		return err
	}
	if semver.Compare(canonicalVersion(launcherRelease.Version), canonicalVersion(config.Version)) > 0 {
		return &LauncherUpdateRequired{Version: launcherRelease.Version, Artifact: launcherRelease.Launcher}
	}
	return nil
}

func launchCurrentRuntime(
	ctx context.Context,
	config Config,
	identity identity,
	store *centralstore.Store,
) error {
	report(config, Progress{Stage: StageChecking, Message: "최신 릴리스 확인 중"})
	release, err := store.CurrentRuntimeRelease(ctx, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("load current client release: %w", err)
	}
	if err := validateReleaseForLauncher(release, config.Version); err != nil {
		return err
	}
	installed, err := installRelease(ctx, config, release)
	if err != nil {
		return err
	}
	rollback := func(cause error) error {
		if rollbackErr := rollbackInstalledRelease(config.DataDir, installed); rollbackErr != nil {
			return errors.Join(cause, fmt.Errorf("rollback Client runtime: %w", rollbackErr))
		}
		return cause
	}
	nonce, err := randomToken(24)
	if err != nil {
		return rollback(err)
	}
	generation := store.ReleaseGeneration()
	if generation <= 0 {
		return rollback(errors.New("central returned no active release generation"))
	}
	ticket, err := store.IssueLaunchTicket(ctx, central.LaunchTicketRequest{
		InstallationID: identity.InstallationID, DeviceID: identity.DeviceID, ReleaseGeneration: generation,
		ClientVersion: release.Client.Version, ArtifactSHA256: release.Client.Artifact.SHA256,
		Protocol: release.Client.Protocol, BrowserRevision: release.Browser.Revision,
		BrowserArtifactSHA256: release.Browser.Artifact.SHA256,
		PlaywrightVersion:     release.Playwright.Version, PlaywrightArtifactSHA256: release.Playwright.Artifact.SHA256,
		Nonce: nonce,
	})
	if err != nil {
		return rollback(fmt.Errorf("issue client launch ticket: %w", err))
	}
	if !ticket.ExpiresAt.After(time.Now()) || ticket.LaunchTicket == "" {
		return rollback(errors.New("central returned an invalid launch ticket"))
	}
	report(config, Progress{Stage: StageLaunching, Message: "Cineko Client 시작 중"})
	if err := runClient(ctx, config, installed, identity, ticket.LaunchTicket, generation); err != nil {
		return rollback(err)
	}
	finalizeInstalledRelease(config.DataDir, installed)
	return nil
}

func validateLauncherRelease(release central.LauncherRelease) error {
	if release.Channel != "stable" || release.Platform != runtime.GOOS || release.Arch != runtime.GOARCH ||
		release.Protocol != central.ProtocolVersion || !semver.IsValid(canonicalVersion(release.Version)) {
		return errors.New("release is incompatible with this launcher")
	}
	if err := validateArtifactMetadata(release.Launcher); err != nil {
		return fmt.Errorf("launcher download: %w", err)
	}
	return nil
}

func report(config Config, progress Progress) {
	if config.OnProgress != nil {
		config.OnProgress(progress)
	}
}

func validateConfig(config Config) error {
	if strings.TrimSpace(config.CentralURL) == "" || strings.TrimSpace(config.DataDir) == "" {
		return errors.New("central URL and launcher data directory are required")
	}
	if err := validateCentralURL(config.CentralURL); err != nil {
		return err
	}
	if !semver.IsValid(canonicalVersion(config.Version)) {
		return errors.New("launcher version must be semantic versioning")
	}
	return nil
}

func validateCentralURL(rawURL string) error {
	endpoint, err := url.ParseRequestURI(strings.TrimSpace(rawURL))
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		(endpoint.Path != "" && endpoint.Path != "/") {
		return errors.New("central URL must be an origin without credentials, path, query, or fragment")
	}
	if endpoint.Scheme == "https" {
		return nil
	}
	hostname := strings.ToLower(endpoint.Hostname())
	address := net.ParseIP(hostname)
	if endpoint.Scheme == "http" && (hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") ||
		address != nil && address.IsLoopback()) {
		return nil
	}
	return errors.New("central URL must use HTTPS unless it targets loopback")
}

func validateReleaseForLauncher(release central.RuntimeRelease, launcherVersion string) error {
	client := release.Client
	if client.Channel != "stable" || release.Browser.Channel != "stable" || release.Playwright.Channel != "stable" ||
		client.Platform != runtime.GOOS || client.Arch != runtime.GOARCH ||
		release.Browser.Platform != runtime.GOOS || release.Browser.Arch != runtime.GOARCH ||
		release.Playwright.Platform != runtime.GOOS || release.Playwright.Arch != runtime.GOARCH ||
		client.Protocol != central.ProtocolVersion {
		return errors.New("release is incompatible with this launcher")
	}
	if err := validateRuntimeCompatibility(release, launcherVersion); err != nil {
		return err
	}
	for _, component := range []struct {
		name     string
		artifact central.ReleaseArtifact
	}{
		{name: "client", artifact: release.Client.Artifact},
		{name: "browser", artifact: release.Browser.Artifact},
		{name: "playwright", artifact: release.Playwright.Artifact},
	} {
		if err := validateArtifactMetadata(component.artifact); err != nil {
			return fmt.Errorf("%s artifact: %w", component.name, err)
		}
	}
	return nil
}

func validateRuntimeCompatibility(release central.RuntimeRelease, launcherVersion string) error {
	client := release.Client
	minimum := canonicalVersion(client.MinimumLauncherVersion)
	if !semver.IsValid(minimum) || semver.Compare(canonicalVersion(launcherVersion), minimum) < 0 {
		return fmt.Errorf("launcher %s is older than required %s", launcherVersion, client.MinimumLauncherVersion)
	}
	if !semver.IsValid(canonicalVersion(client.Version)) ||
		!semver.IsValid(canonicalVersion(release.Playwright.Version)) ||
		!validNumericRevision(release.Browser.Revision) ||
		!validNumericRevision(client.MinimumBrowserRevision) ||
		client.PlaywrightVersion != release.Playwright.Version ||
		compareNumericRevision(release.Browser.Revision, client.MinimumBrowserRevision) < 0 ||
		!containsString(release.Browser.CompatiblePlaywrightVersions, release.Playwright.Version) {
		return errors.New("release client version is invalid")
	}
	return nil
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

func validateArtifactMetadata(artifact central.ReleaseArtifact) error {
	parsed, err := url.Parse(strings.TrimSpace(artifact.URL))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" ||
		artifact.Size <= 0 {
		return errors.New("HTTPS URL and positive size are required")
	}
	digest, err := hex.DecodeString(strings.TrimSpace(artifact.SHA256))
	if err != nil || len(digest) != sha256.Size {
		return errors.New("SHA-256 must contain 64 hexadecimal characters")
	}
	executable := strings.TrimSpace(artifact.Executable)
	if executable == "" || path.IsAbs(executable) || path.Clean(executable) != executable ||
		strings.HasPrefix(executable, "../") {
		return errors.New("executable must be a clean relative archive path")
	}
	return nil
}

func runClient(
	ctx context.Context,
	config Config,
	installed installedRelease,
	identity identity,
	launchTicket string,
	releaseGeneration int64,
) error {
	payload, err := json.Marshal(central.ClientLaunchEnvelope{
		LaunchTicket: launchTicket,
		ClientLaunchContext: central.ClientLaunchContext{
			InstallationID: identity.InstallationID, DeviceID: identity.DeviceID,
			ReleaseGeneration: releaseGeneration,
			ClientVersion:     installed.Release.Client.Version, ArtifactSHA256: installed.Release.Client.Artifact.SHA256,
			Protocol: installed.Release.Client.Protocol, BrowserRevision: installed.Release.Browser.Revision,
			BrowserArtifactSHA256:    installed.Release.Browser.Artifact.SHA256,
			PlaywrightVersion:        installed.Release.Playwright.Version,
			PlaywrightArtifactSHA256: installed.Release.Playwright.Artifact.SHA256,
		},
	})
	if err != nil {
		return fmt.Errorf("encode client launch payload: %w", err)
	}
	command := exec.CommandContext(ctx, installed.ClientPath) // #nosec G204 -- path is hash-verified release metadata.
	command.Stdin = strings.NewReader(string(payload))
	command.Stdout = defaultWriter(config.Stdout, os.Stdout)
	command.Stderr = defaultWriter(config.Stderr, os.Stderr)
	command.Env = append(sanitizedEnvironment(os.Environ()),
		"CINEKO_CENTRAL_URL="+config.CentralURL,
		"CINEKO_CHROME_PATH="+installed.BrowserPath,
		"CINEKO_PLAYWRIGHT_DRIVER_PATH="+installed.DriverPath,
		"CINEKO_PROBE_BOOTSTRAP_PUBLIC_KEYS="+installed.ProbePublicKeySpec,
	)
	if err := command.Start(); err != nil {
		return fmt.Errorf("run Cineko Client: %w", err)
	}
	report(config, Progress{Stage: StageRunning, Message: "Cineko Client 실행 중"})
	if config.OnClientStarted != nil {
		config.OnClientStarted()
	}
	if err := command.Wait(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == clientUpdateRequiredExitCode {
			return fmt.Errorf("client detected a newer release generation: %w", centralstore.ErrReleaseChanged)
		}
		return fmt.Errorf("wait for Cineko Client: %w", err)
	}
	return nil
}

func compareNumericRevision(left string, right string) int {
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

func sanitizedEnvironment(environment []string) []string {
	blocked := map[string]struct{}{
		"CINEKO_CENTRAL_ACCESS_TOKEN": {},
		"CINEKO_CENTRAL_USER_ID":      {},
		"CINEKO_DEV_DIRECT":           {},
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
	path := filepath.Join(dataDir, "installation.json")
	contents, err := os.ReadFile(path) // #nosec G304 -- path is scoped to launcher data directory.
	if err == nil {
		var value identity
		if json.Unmarshal(contents, &value) != nil || value.InstallationID == "" || value.DeviceID == "" {
			return identity{}, errors.New("launcher installation identity is invalid")
		}
		return value, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return identity{}, fmt.Errorf("read launcher installation identity: %w", err)
	}
	installation, err := randomToken(16)
	if err != nil {
		return identity{}, err
	}
	device, err := randomToken(16)
	if err != nil {
		return identity{}, err
	}
	value := identity{InstallationID: "install_" + installation, DeviceID: "device_" + device}
	if err := writeJSONAtomic(path, value); err != nil {
		return identity{}, err
	}
	return value, nil
}
