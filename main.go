package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/cineko-org/launcher/internal/desktop"
	"github.com/cineko-org/launcher/internal/launcher"
	"github.com/cineko-org/launcher/internal/telemetry"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

var (
	launcherVersion        = "0.0.0-dev"
	launcherReleaseBaseURL = "https://github.com/cineko-org"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "cineko-launcher: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if bindingMode {
		return wails.Run(&options.App{Bind: []interface{}{&desktop.Launcher{}}})
	}
	dataDir, err := launcherDataDir()
	if err != nil {
		return err
	}
	debugMode := launcherDebugMode()
	logger, closeLog, err := launcherLogger(dataDir, debugMode)
	if err != nil {
		return err
	}
	defer closeLog()
	app := desktop.New(launcher.Config{
		ReleaseBaseURL: resolvedReleaseBaseURL(),
		ClientPath:     strings.TrimSpace(os.Getenv("CINEKO_CLIENT_PATH")),
		ChromePath:     strings.TrimSpace(os.Getenv("CINEKO_CHROME_PATH")),
		DriverPath:     strings.TrimSpace(os.Getenv("CINEKO_PLAYWRIGHT_DRIVER_PATH")),
		DataDir:        dataDir,
		Version:        launcherVersion,
		Debug:          debugMode,
	}, logger)
	return wails.Run(&options.App{
		Title:             "Cineko Launcher",
		Width:             720,
		Height:            560,
		MinWidth:          360,
		MinHeight:         520,
		HideWindowOnClose: runtime.GOOS == "darwin",
		BackgroundColour:  options.NewRGB(10, 11, 14),
		AssetServer: &assetserver.Options{
			Assets:     launcher.Assets(),
			Middleware: telemetry.HTTPServerMiddleware(logger),
		},
		OnStartup:  app.Startup,
		OnDomReady: app.Ready,
		OnShutdown: app.Shutdown,
		Bind:       []interface{}{app},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               "io.cineko.launcher",
			OnSecondInstanceLaunch: func(options.SecondInstanceData) { app.Show() },
		},
		Mac: &mac.Options{
			Appearance: mac.NSAppearanceNameDarkAqua,
			About:      &mac.AboutInfo{Title: "Cineko Launcher", Message: "Cineko 실행 및 업데이트"},
		},
		Windows: &windows.Options{WebviewUserDataPath: filepath.Join(dataDir, "webview-launcher")},
	})
}

func resolvedReleaseBaseURL() string {
	if value := strings.TrimSpace(os.Getenv("CINEKO_RELEASE_BASE_URL")); value != "" {
		return value
	}
	return strings.TrimSpace(launcherReleaseBaseURL)
}

func launcherLogger(dataDir string, debug bool) (*slog.Logger, func(), error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create launcher data directory: %w", err)
	}
	file, err := os.OpenFile( // #nosec G304 -- path is scoped to the resolved Launcher data directory.
		filepath.Join(dataDir, "launcher.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("open launcher log: %w", err)
	}
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(file, &slog.HandlerOptions{Level: level}))
	return logger, func() { _ = file.Close() }, nil
}

func launcherDebugMode() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CINEKO_DEBUG"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func launcherDataDir() (string, error) {
	return resolveLauncherDataDir(os.Getenv("CINEKO_DATA_DIR"), os.UserHomeDir)
}

func resolveLauncherDataDir(configured string, homeDir func() (string, error)) (string, error) {
	if dataDir := strings.TrimSpace(configured); dataDir != "" {
		if !filepath.IsAbs(dataDir) {
			return "", errors.New("CINEKO_DATA_DIR must be an absolute path")
		}
		return filepath.Clean(dataDir), nil
	}
	root, err := homeDir()
	if err != nil {
		return "", fmt.Errorf("find launcher home directory: %w", err)
	}
	return filepath.Join(root, "cineko"), nil
}
