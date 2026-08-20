package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cineko-org/launcher/internal/desktop"
	"github.com/cineko-org/launcher/internal/launcher"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

var (
	launcherVersion    = "0.0.0-dev"
	launcherCentralURL string
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
	logger, closeLog, err := launcherLogger(dataDir)
	if err != nil {
		return err
	}
	defer closeLog()
	app := desktop.New(launcher.Config{
		CentralURL: resolvedCentralURL(),
		DataDir:    dataDir,
		Version:    launcherVersion,
	}, logger)
	return wails.Run(&options.App{
		Title:            "Cineko Launcher",
		Width:            720,
		Height:           560,
		MinWidth:         360,
		MinHeight:        520,
		BackgroundColour: options.NewRGB(10, 11, 14),
		AssetServer:      &assetserver.Options{Assets: launcher.Assets()},
		OnStartup:        app.Startup,
		Bind:             []interface{}{app},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               "io.cineko.launcher",
			OnSecondInstanceLaunch: func(options.SecondInstanceData) { app.Show() },
		},
		Mac: &mac.Options{
			Appearance: mac.NSAppearanceNameDarkAqua,
			About:      &mac.AboutInfo{Title: "Cineko Launcher", Message: "Cineko 인증 및 업데이트"},
		},
	})
}

func resolvedCentralURL() string {
	if value := strings.TrimSpace(os.Getenv("CINEKO_CENTRAL_URL")); value != "" {
		return value
	}
	return strings.TrimSpace(launcherCentralURL)
}

func launcherLogger(dataDir string) (*slog.Logger, func(), error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create launcher data directory: %w", err)
	}
	file, err := os.OpenFile( // #nosec G304 -- path is scoped to the resolved Launcher data directory.
		filepath.Join(dataDir, "launcher.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("open launcher log: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(file, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return logger, func() { _ = file.Close() }, nil
}

func launcherDataDir() (string, error) {
	if dataDir := strings.TrimSpace(os.Getenv("CINEKO_DATA_DIR")); dataDir != "" {
		return dataDir, nil
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find launcher data directory: %w", err)
	}
	return filepath.Join(root, "Cineko"), nil
}
