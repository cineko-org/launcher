package launcher

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	central "github.com/cineko-org/contracts/v3"
)

func TestLauncherAuthenticatesWithPINAndResumesSession(t *testing.T) {
	now := time.Now().UTC()
	exchanges := 0
	centralServer := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set(central.ReleaseGenerationHeader, "11")
		switch request.URL.Path {
		case "/v1/auth/pin":
			exchanges++
			var input central.ClientPINExchangeRequest
			_ = json.NewDecoder(request.Body).Decode(&input)
			if input.PIN != "123456" || input.InstallationID != "install_1234567890" ||
				input.DeviceID != "device_123456789012" {
				writer.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(writer).Encode(central.AuthExchangeResponse{ // #nosec G117 -- test-only fixture.
				AccessToken: "access", ExpiresAt: now.Add(time.Hour),
				RefreshToken: "refresh", RefreshExpiresAt: now.Add(24 * time.Hour),
				User: central.ClientUser{ID: "user"},
			})
		case "/v1/client/bootstrap":
			_ = json.NewEncoder(writer).Encode(central.ClientBootstrap{User: central.ClientUser{ID: "user"}})
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(centralServer.Close)
	dataDir := t.TempDir()
	launcherIdentity := identity{InstallationID: "install_1234567890", DeviceID: "device_123456789012"}
	config := Config{
		CentralURL: centralServer.URL, DataDir: dataDir, PIN: "123456", HTTPClient: centralServer.Client(),
	}
	store, err := authenticateLauncher(t.Context(), config, launcherIdentity)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	if exchanges != 1 {
		t.Fatalf("PIN exchanges = %d", exchanges)
	}
	info, err := os.Stat(launcherSessionPath(dataDir))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("launcher session permissions = %v, %v", info, err)
	}
	config.PIN = ""
	store, err = authenticateLauncher(t.Context(), config, launcherIdentity)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	if exchanges != 1 {
		t.Fatalf("resumed session exchanged PIN %d times", exchanges)
	}
	if err := Logout(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	if err := Logout(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(launcherSessionPath(dataDir)); !os.IsNotExist(err) {
		t.Fatalf("launcher session still exists: %v", err)
	}
}

func TestLauncherRequiresValidPINWithoutSession(t *testing.T) {
	config := Config{CentralURL: "https://central.example", DataDir: t.TempDir()}
	identity := identity{InstallationID: "install", DeviceID: "device"}
	if _, err := authenticateLauncher(t.Context(), config, identity); !errors.Is(err, ErrAuthenticationRequired) {
		t.Fatalf("missing PIN error = %v", err)
	}
	for _, pin := range []string{"12345", "12345a", "１２３４５６"} {
		config.PIN = pin
		if _, err := authenticateLauncher(t.Context(), config, identity); !errors.Is(err, ErrInvalidPIN) {
			t.Fatalf("invalid PIN %q error = %v", pin, err)
		}
	}
	if !validPIN("123456") || validPIN("12345a") || validPIN("12345") {
		t.Fatal("PIN validation mismatch")
	}
}

func TestLauncherChecksCentralHealthBeforeRequestingPIN(t *testing.T) {
	healthChecks := 0
	centralServer := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set(central.ReleaseGenerationHeader, "11")
		if request.URL.Path != "/health" {
			t.Fatalf("unexpected request before authentication: %s", request.URL.Path)
		}
		healthChecks++
		_ = json.NewEncoder(writer).Encode(map[string]string{"status": "ready"})
	}))
	t.Cleanup(centralServer.Close)
	err := Run(t.Context(), Config{
		CentralURL: centralServer.URL, DataDir: t.TempDir(), Version: "1.0.0", HTTPClient: centralServer.Client(),
	})
	if !errors.Is(err, ErrAuthenticationRequired) {
		t.Fatalf("missing PIN error = %v", err)
	}
	if healthChecks != 1 {
		t.Fatalf("health checks = %d", healthChecks)
	}
}
