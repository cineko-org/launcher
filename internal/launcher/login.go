package launcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	clientpb "github.com/cineko-org/contracts/v3/gen/go/cineko/client"
	centralstore "github.com/cineko-org/launcher/internal/centralclient"
	"github.com/cineko-org/launcher/internal/launcher/managedfiles"

	"google.golang.org/protobuf/encoding/protojson"
)

var (
	ErrAuthenticationRequired = errors.New("launcher authentication is required")
	ErrInvalidPIN             = errors.New("PIN must contain six digits")
)

func authenticateLauncher(ctx context.Context, config Config, identity identity) (*centralstore.Store, error) {
	onSessionChanged := func(session *clientpb.AuthenticationResponse) error {
		return saveLauncherSession(config.DataDir, session)
	}
	if strings.TrimSpace(config.UserID) != "" && strings.TrimSpace(config.AccessToken) != "" {
		return centralstore.Open(ctx, centralstore.Config{
			BaseURL: config.CentralURL, UserID: config.UserID,
			AccessToken: config.AccessToken, HTTPClient: config.HTTPClient, SessionChanged: onSessionChanged,
		})
	}
	if store, err := resumeLauncherSession(ctx, config, identity.InstallationID); err == nil {
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
		HTTPClient: config.HTTPClient, SessionChanged: onSessionChanged,
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

func resumeLauncherSession(ctx context.Context, config Config, installationID string) (*centralstore.Store, error) {
	contents, err := os.ReadFile(launcherSessionPath(config.DataDir)) // #nosec G304 -- scoped launcher state path.
	if err != nil {
		return nil, err
	}
	session := &clientpb.AuthenticationResponse{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(contents, session); err != nil {
		return nil, errors.New("launcher session is invalid")
	}
	if session.GetUser() == nil || session.GetUser().GetId() == "" || session.GetAccessToken() == "" ||
		session.GetExpiresAt() == nil || session.GetRefreshToken() == "" || session.GetRefreshExpiresAt() == nil {
		return nil, errors.New("launcher session is invalid")
	}
	store, err := centralstore.Resume(centralstore.SessionConfig{
		BaseURL: config.CentralURL, UserID: session.GetUser().GetId(),
		AccessToken: session.GetAccessToken(), ExpiresAt: session.GetExpiresAt().AsTime(),
		RefreshToken: session.GetRefreshToken(), RefreshExpiresAt: session.GetRefreshExpiresAt().AsTime(),
		HTTPClient: config.HTTPClient,
		SessionChanged: func(session *clientpb.AuthenticationResponse) error {
			return saveLauncherSession(config.DataDir, session)
		},
	})
	if err != nil {
		return nil, err
	}
	if err := store.ValidateSession(ctx, installationID); err != nil {
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

func saveLauncherSession(dataDir string, value *clientpb.AuthenticationResponse) error {
	if value == nil || value.GetUser() == nil || value.GetUser().GetId() == "" || value.GetAccessToken() == "" ||
		value.GetExpiresAt() == nil || value.GetRefreshToken() == "" || value.GetRefreshExpiresAt() == nil {
		return errors.New("cannot persist invalid launcher session")
	}
	contents, err := (protojson.MarshalOptions{UseProtoNames: false}).Marshal(value)
	if err != nil {
		return err
	}
	return managedfiles.WriteAtomic(launcherSessionPath(dataDir), contents)
}

func launcherSessionPath(dataDir string) string { return filepath.Join(dataDir, "session.json") }

func Logout(ctx context.Context, config Config) error {
	if identity, err := loadIdentity(config.DataDir); err == nil {
		if store, resumeErr := resumeLauncherSession(ctx, config, identity.InstallationID); resumeErr == nil {
			_ = store.Logout(ctx)
			_ = store.Close()
		}
	}
	err := os.Remove(launcherSessionPath(config.DataDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
