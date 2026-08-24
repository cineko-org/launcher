package desktop

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"os/exec"
	"path/filepath"
	stdruntime "runtime"
	"strings"
	"sync"

	"github.com/cineko-org/launcher/internal/launcher"
	launcherartifact "github.com/cineko-org/launcher/internal/launcher/artifact"
	"github.com/cineko-org/launcher/internal/telemetry"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

type Mode string

const (
	ModeChecking       Mode = "checking"
	ModeUpdating       Mode = "updating"
	ModeLauncherUpdate Mode = "launcher-update"
	ModeLaunching      Mode = "launching"
	ModeError          Mode = "error"
)

type State struct {
	Revision      uint64         `json:"revision"`
	Mode          Mode           `json:"mode"`
	Stage         launcher.Stage `json:"stage,omitempty"`
	Message       string         `json:"message"`
	Artifact      string         `json:"artifact,omitempty"`
	Downloaded    int64          `json:"downloaded,omitempty"`
	Total         int64          `json:"total,omitempty"`
	Version       string         `json:"version"`
	LatestVersion string         `json:"latestVersion,omitempty"`
	DownloadURL   string         `json:"downloadUrl,omitempty"`
}

type Launcher struct {
	mu      sync.RWMutex
	ctx     context.Context
	config  launcher.Config
	state   State
	running bool
	logger  *slog.Logger
	update  *launcher.LauncherUpdateRequired
}

func New(config launcher.Config, logger *slog.Logger) *Launcher {
	if config.Logger == nil {
		config.Logger = logger
	}
	return &Launcher{
		config: config,
		state:  State{Revision: 1, Mode: ModeChecking, Message: "Cineko 시작 준비 중", Version: config.Version},
		logger: logger,
	}
}

func (app *Launcher) Startup(ctx context.Context) {
	app.mu.Lock()
	app.ctx = ctx
	app.mu.Unlock()
	_ = app.start()
}

func (app *Launcher) State() State {
	app.mu.RLock()
	defer app.mu.RUnlock()
	return app.state
}

func (app *Launcher) Retry() error { return app.start() }

func (app *Launcher) Quit() {
	app.mu.RLock()
	ctx := app.ctx
	app.mu.RUnlock()
	if ctx != nil {
		runtime.Quit(ctx)
	}
}

func (app *Launcher) DownloadLauncher() error {
	app.mu.RLock()
	ctx, state, update, config := app.ctx, app.state, app.update, app.config
	app.mu.RUnlock()
	if ctx == nil || state.Mode != ModeLauncherUpdate || update == nil || state.DownloadURL == "" {
		return errors.New("Launcher download is unavailable")
	}
	ctx = app.requestContext(ctx, config)
	parsed, err := url.Parse(update.Artifact.GetUrl())
	if err != nil {
		return err
	}
	destination, err := runtime.SaveFileDialog(ctx, runtime.SaveDialogOptions{
		DefaultFilename: filepath.Base(parsed.Path),
		Title:           "새 Cineko Launcher 저장",
	})
	if err != nil || destination == "" {
		return err
	}
	app.publish(State{
		Mode: ModeUpdating, Stage: launcher.StageDownloading, Message: "새 Launcher 다운로드 중",
		Artifact: "launcher", Version: config.Version, Total: update.Artifact.GetSize(),
	})
	if err := launcherartifact.DownloadPortableLauncher(
		ctx, config.HTTPClient, filepath.Join(config.DataDir, "downloads"), update.Artifact, destination,
		func(downloaded int64) {
			app.publish(State{
				Mode: ModeUpdating, Stage: launcher.StageDownloading, Message: "새 Launcher 다운로드 중",
				Artifact: "launcher", Version: config.Version, Downloaded: downloaded, Total: update.Artifact.GetSize(),
			})
		},
	); err != nil {
		app.publishFailure(config, err)
		return err
	}
	app.mu.Lock()
	app.update = update
	app.mu.Unlock()
	app.publish(State{
		Mode: ModeLauncherUpdate, Message: "새 Launcher를 저장했습니다. 현재 Launcher를 종료한 뒤 실행하세요.",
		Version: config.Version, LatestVersion: update.Version, DownloadURL: update.Artifact.GetUrl(),
	})
	if err := revealFile(ctx, destination); err != nil {
		if app.logger != nil {
			app.logger.Warn("reveal downloaded Launcher failed", "path", destination, "error", err)
		}
	}
	return nil
}

func (app *Launcher) requestContext(ctx context.Context, config launcher.Config) context.Context {
	logger := app.logger
	if logger == nil {
		logger = config.Logger
	}
	return telemetry.WithLogger(ctx, logger)
}

func (app *Launcher) Show() {
	app.mu.RLock()
	ctx := app.ctx
	app.mu.RUnlock()
	if ctx != nil {
		runtime.WindowShow(ctx)
		runtime.WindowUnminimise(ctx)
	}
}

func (app *Launcher) start() error {
	app.mu.Lock()
	if app.running {
		app.mu.Unlock()
		return errors.New("launcher is already running")
	}
	app.running = true
	config, ctx := app.config, app.ctx
	app.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	config.OnProgress = app.progress
	config.OnClientStarted = app.clientStarted
	go app.execute(ctx, config)
	return nil
}

func (app *Launcher) execute(ctx context.Context, config launcher.Config) {
	err := launcher.Run(ctx, config)
	app.mu.Lock()
	app.running = false
	app.mu.Unlock()
	if err == nil {
		runtime.Quit(ctx)
		return
	}
	if app.logger != nil {
		app.logger.Error("launcher run failed", "error", err)
	}
	app.Show()
	app.publishFailure(config, err)
}

func (app *Launcher) publishFailure(config launcher.Config, err error) {
	var update *launcher.LauncherUpdateRequired
	switch {
	case errors.As(err, &update):
		app.mu.Lock()
		app.update = update
		app.mu.Unlock()
		app.publish(State{
			Mode: ModeLauncherUpdate, Message: "계속하려면 새 Launcher를 내려받아 실행하세요.",
			Version: config.Version, LatestVersion: update.Version, DownloadURL: update.Artifact.GetUrl(),
		})
	default:
		app.publish(State{Mode: ModeError, Message: userFacingError(err), Version: config.Version})
	}
}

func revealFile(ctx context.Context, path string) error {
	var command *exec.Cmd
	switch stdruntime.GOOS {
	case "darwin":
		command = exec.CommandContext(ctx, "open", "-R", path) // #nosec G204 -- fixed command with one local path argument.
	case "windows":
		command = exec.CommandContext(ctx, "explorer.exe", "/select,"+path) // #nosec G204 -- fixed command with one local path argument.
	default:
		command = exec.CommandContext(ctx, "xdg-open", filepath.Dir(path)) // #nosec G204 -- fixed command with one local path argument.
	}
	return command.Start()
}

func (app *Launcher) progress(progress launcher.Progress) {
	mode := ModeUpdating
	if progress.Stage == launcher.StageLaunching || progress.Stage == launcher.StageRunning {
		mode = ModeLaunching
	}
	app.publish(State{Mode: mode, Stage: progress.Stage, Message: progress.Message, Artifact: progress.Artifact, Downloaded: progress.Downloaded, Total: progress.Total, Version: app.config.Version})
}

func (app *Launcher) clientStarted() {
	app.mu.RLock()
	ctx := app.ctx
	app.mu.RUnlock()
	if ctx != nil {
		runtime.WindowHide(ctx)
	}
}

func (app *Launcher) publish(state State) {
	app.mu.Lock()
	state.Revision = app.state.Revision + 1
	app.state = state
	ctx := app.ctx
	app.mu.Unlock()
	if ctx != nil {
		runtime.EventsEmit(ctx, "launcher:state", state)
	}
}

func userFacingError(err error) string {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "download"), strings.Contains(message, "artifact"):
		return "업데이트 파일을 받지 못했습니다. 네트워크 연결을 확인한 뒤 다시 시도하세요."
	case strings.Contains(message, "connect"), strings.Contains(message, "dial tcp"), strings.Contains(message, "release manifest"):
		return "업데이트 서버에 연결할 수 없습니다. 네트워크 연결을 확인한 뒤 다시 시도하세요."
	default:
		return "Cineko를 시작할 수 없습니다. 잠시 후 다시 시도해 주세요."
	}
}
