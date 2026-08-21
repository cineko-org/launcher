package launcher

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	clientpb "github.com/cineko-org/contracts/gen/go/cineko/client"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestLauncherAuthenticatesWithPINAndResumesSession(t *testing.T) {
	now := time.Now().UTC()
	exchanges := 0
	centralServer := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Cineko-Release-Generation", "11")
		switch request.URL.Path {
		case "/v1/auth/pin":
			exchanges++
			input := &clientpb.PinExchangeRequest{}
			if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(readBody(t, request), input); err != nil {
				t.Fatalf("decode PIN request: %v", err)
			}
			if input.GetPin() != "123456" || input.GetInstallationId() != "install_1234567890" ||
				input.GetDeviceId() != "device_123456789012" {
				writer.WriteHeader(http.StatusUnauthorized)
				return
			}
			writeProtoJSON(t, writer, authenticationResponse(now, "access"))
		case "/v1/client/bootstrap":
			bootstrap := &clientpb.Bootstrap{}
			user := &clientpb.User{}
			user.SetId("user")
			bootstrap.SetUser(user)
			writeProtoJSON(t, writer, bootstrap)
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
		writer.Header().Set("X-Cineko-Release-Generation", "11")
		if request.URL.Path != "/health" {
			t.Fatalf("unexpected request before authentication: %s", request.URL.Path)
		}
		healthChecks++
		writeProtoJSON(t, writer, serviceHealthReady())
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
