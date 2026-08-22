package launcher

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	centralstore "github.com/cineko-org/launcher/internal/centralclient"
	"github.com/cineko-org/launcher/internal/launcher/artifact"
	"github.com/cineko-org/launcher/internal/launcher/managedfiles"

	"golang.org/x/mod/semver"
	"google.golang.org/protobuf/encoding/protojson"
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
	Artifact *releasepb.Artifact
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
	Release            *releasepb.RuntimeRelease `json:"release"`
	ClientPath         string                    `json:"clientPath"`
	BrowserPath        string                    `json:"browserPath"`
	DriverPath         string                    `json:"driverPath"`
	ProbePublicKeyHash string                    `json:"probePublicKeyHash"`
	ProbePublicKeySpec string                    `json:"probePublicKeySpec"`
	Previous           *installedRelease         `json:"-"`
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
	device := &clientpb.Device{}
	device.SetInstallationId(identity.InstallationID)
	device.SetDeviceId(identity.DeviceID)
	device.SetPlatform(runtime.GOOS)
	device.SetArchitecture(runtime.GOARCH)
	device.SetAppVersion("launcher/" + config.Version)
	if _, err := store.RegisterDevice(ctx, device); err != nil {
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
	if semver.Compare(canonicalVersion(launcherRelease.GetVersion()), canonicalVersion(config.Version)) > 0 {
		return &LauncherUpdateRequired{Version: launcherRelease.GetVersion(), Artifact: launcherRelease.GetLauncher()}
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
	launchContext := &clientpb.LaunchContext{}
	launchContext.SetInstallationId(identity.InstallationID)
	launchContext.SetDeviceId(identity.DeviceID)
	launchContext.SetReleaseGeneration(generation)
	launchContext.SetClientVersion(release.GetClient().GetVersion())
	launchContext.SetArtifactSha256(release.GetClient().GetArtifact().GetSha256())
	launchContext.SetBrowserRevision(release.GetBrowser().GetRevision())
	launchContext.SetBrowserArtifactSha256(release.GetBrowser().GetArtifact().GetSha256())
	launchContext.SetPlaywrightVersion(release.GetPlaywright().GetVersion())
	launchContext.SetPlaywrightArtifactSha256(release.GetPlaywright().GetArtifact().GetSha256())
	ticketRequest := &clientpb.LaunchTicketRequest{}
	ticketRequest.SetContext(launchContext)
	ticketRequest.SetNonce(nonce)
	ticket, err := store.IssueLaunchTicket(ctx, ticketRequest)
	if err != nil {
		return rollback(fmt.Errorf("issue client launch ticket: %w", err))
	}
	if ticket.GetExpiresAt() == nil || !ticket.GetExpiresAt().AsTime().After(time.Now()) || ticket.GetLaunchTicket() == "" {
		return rollback(errors.New("central returned an invalid launch ticket"))
	}
	report(config, Progress{Stage: StageLaunching, Message: "Cineko Client 시작 중"})
	ready, err := runClient(ctx, config, installed, identity, ticket.GetLaunchTicket(), generation, func() {
		finalizeInstalledRelease(config.DataDir, installed)
	})
	if err != nil {
		if !ready {
			return rollback(err)
		}
		return err
	}
	return nil
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

//nolint:gocyclo,cyclop // Runtime compatibility keeps every generated release component invariant explicit.
func validateReleaseForLauncher(release *releasepb.RuntimeRelease, launcherVersion string) error {
	client := release.GetClient()
	browser := release.GetBrowser()
	playwright := release.GetPlaywright()
	if release == nil || client == nil || browser == nil || playwright == nil ||
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
	}{
		{name: "client", artifact: client.GetArtifact()},
		{name: "browser", artifact: browser.GetArtifact()},
		{name: "playwright", artifact: playwright.GetArtifact()},
	} {
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

func runClient(
	ctx context.Context,
	config Config,
	installed installedRelease,
	identity identity,
	launchTicket string,
	releaseGeneration int64,
	onReady func(),
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
	launchContext := &clientpb.LaunchContext{}
	launchContext.SetInstallationId(identity.InstallationID)
	launchContext.SetDeviceId(identity.DeviceID)
	launchContext.SetReleaseGeneration(releaseGeneration)
	launchContext.SetClientVersion(installed.Release.GetClient().GetVersion())
	launchContext.SetArtifactSha256(installed.Release.GetClient().GetArtifact().GetSha256())
	launchContext.SetBrowserRevision(installed.Release.GetBrowser().GetRevision())
	launchContext.SetBrowserArtifactSha256(installed.Release.GetBrowser().GetArtifact().GetSha256())
	launchContext.SetPlaywrightVersion(installed.Release.GetPlaywright().GetVersion())
	launchContext.SetPlaywrightArtifactSha256(installed.Release.GetPlaywright().GetArtifact().GetSha256())
	envelope := &clientpb.LaunchEnvelope{}
	envelope.SetLaunchTicket(launchTicket)
	envelope.SetContext(launchContext)
	payload, err := protojson.MarshalOptions{UseProtoNames: false}.Marshal(envelope)
	if err != nil {
		return false, fmt.Errorf("encode client launch payload: %w", err)
	}
	processContext, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(processContext, installed.ClientPath) // #nosec G204 -- path is hash-verified release metadata.
	command.Stdin = strings.NewReader(string(payload))
	command.Stdout = defaultWriter(config.Stdout, os.Stdout)
	command.Stderr = defaultWriter(config.Stderr, os.Stderr)
	command.Env = append(sanitizedEnvironment(os.Environ()),
		"CINEKO_CENTRAL_URL="+config.CentralURL,
		"CINEKO_DATA_DIR="+config.DataDir,
		"CINEKO_STARTUP_READY_NONCE="+startupNonce,
		"CINEKO_CHROME_PATH="+installed.BrowserPath,
		"CINEKO_PLAYWRIGHT_DRIVER_PATH="+installed.DriverPath,
		"CINEKO_PROBE_BOOTSTRAP_PUBLIC_KEYS="+installed.ProbePublicKeySpec,
	)
	if err := command.Start(); err != nil {
		return false, fmt.Errorf("run Cineko Client: %w", err)
	}
	processDone := make(chan error, 1)
	go func() { processDone <- command.Wait() }()
	if err := awaitStartupReady(
		ctx, startupMarker, startupNonce, processDone, clientStartupTimeout, startupCheckInterval,
	); err != nil {
		cancel()
		_ = command.Process.Kill()
		select {
		case <-processDone:
		case <-time.After(time.Second):
		}
		return false, classifyClientExit(err)
	}
	if onReady != nil {
		onReady()
	}
	report(config, Progress{Stage: StageRunning, Message: "Cineko Client 실행 중"})
	if config.OnClientStarted != nil {
		config.OnClientStarted()
	}
	if err := <-processDone; err != nil {
		return true, classifyClientExit(err)
	}
	return true, nil
}

func classifyClientExit(err error) error {
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == clientUpdateRequiredExitCode {
		return fmt.Errorf("client detected a newer release generation: %w", centralstore.ErrReleaseChanged)
	}
	return fmt.Errorf("wait for Cineko Client: %w", err)
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
		"CINEKO_DATA_DIR":             {},
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
	if err := managedfiles.WriteJSONAtomic(path, value); err != nil {
		return identity{}, err
	}
	return value, nil
}
