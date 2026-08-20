package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	central "github.com/cineko-org/contracts/v3"
	centralstore "github.com/cineko-org/launcher/internal/centralclient"
	"github.com/cineko-org/launcher/internal/launcher/managedfiles"
)

var (
	ErrAuthenticationRequired = errors.New("launcher authentication is required")
	ErrInvalidPIN             = errors.New("PIN must contain six digits")
)

type launcherSession struct {
	UserID           string    `json:"userId"`
	AccessToken      string    `json:"accessToken"`
	ExpiresAt        time.Time `json:"expiresAt"`
	RefreshToken     string    `json:"refreshToken"`
	RefreshExpiresAt time.Time `json:"refreshExpiresAt"`
}

func authenticateLauncher(ctx context.Context, config Config, identity identity) (*centralstore.Store, error) {
	if strings.TrimSpace(config.UserID) != "" && strings.TrimSpace(config.AccessToken) != "" {
		return centralstore.Open(ctx, centralstore.Config{
			BaseURL: config.CentralURL, UserID: config.UserID,
			AccessToken: config.AccessToken, HTTPClient: config.HTTPClient,
		})
	}
	if store, err := resumeLauncherSession(ctx, config); err == nil {
		return store, nil
	}
	pin := strings.TrimSpace(config.PIN)
	if pin == "" {
		return nil, ErrAuthenticationRequired
	}
	if !validPIN(pin) {
		return nil, ErrInvalidPIN
	}
	store, err := centralstore.OpenPIN(ctx, centralstore.PINConfig{
		BaseURL: config.CentralURL, PIN: pin,
		InstallationID: identity.InstallationID, DeviceID: identity.DeviceID,
		HTTPClient: config.HTTPClient,
	})
	if err != nil {
		return nil, err
	}
	if err := saveLauncherSession(config.DataDir, store.Session()); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func validPIN(pin string) bool {
	if len(pin) != 6 {
		return false
	}
	for _, value := range pin {
		if value < '0' || value > '9' {
			return false
		}
	}
	return true
}

func resumeLauncherSession(ctx context.Context, config Config) (*centralstore.Store, error) {
	contents, err := os.ReadFile(launcherSessionPath(config.DataDir)) // #nosec G304 -- scoped launcher state path.
	if err != nil {
		return nil, err
	}
	var session launcherSession
	if json.Unmarshal(contents, &session) != nil {
		return nil, errors.New("launcher session is invalid")
	}
	store, err := centralstore.Resume(centralstore.SessionConfig{
		BaseURL: config.CentralURL, UserID: session.UserID,
		AccessToken: session.AccessToken, ExpiresAt: session.ExpiresAt,
		RefreshToken: session.RefreshToken, RefreshExpiresAt: session.RefreshExpiresAt,
		HTTPClient: config.HTTPClient,
	})
	if err != nil {
		return nil, err
	}
	if err := store.ValidateSession(ctx); err != nil {
		_ = store.Close()
		_ = os.Remove(launcherSessionPath(config.DataDir))
		return nil, err
	}
	if err := saveLauncherSession(config.DataDir, store.Session()); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func saveLauncherSession(dataDir string, value central.AuthExchangeResponse) error {
	return managedfiles.WriteJSONAtomic(launcherSessionPath(dataDir), launcherSession{
		UserID: value.User.ID, AccessToken: value.AccessToken, ExpiresAt: value.ExpiresAt,
		RefreshToken: value.RefreshToken, RefreshExpiresAt: value.RefreshExpiresAt,
	})
}

func launcherSessionPath(dataDir string) string { return filepath.Join(dataDir, "session.json") }

func Logout(ctx context.Context, config Config) error {
	if store, err := resumeLauncherSession(ctx, config); err == nil {
		_ = store.Logout(ctx)
		_ = store.Close()
	}
	err := os.Remove(launcherSessionPath(config.DataDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
