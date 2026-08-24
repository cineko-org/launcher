package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cineko-org/launcher/internal/launcher"
	"github.com/cineko-org/launcher/internal/telemetry"
	wailsassetserver "github.com/wailsapp/wails/v2/pkg/assetserver"
	assetoptions "github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

func TestMacOSLauncherOwnsTheUserFacingApplicationIdentity(t *testing.T) {
	contents, err := os.ReadFile("build/darwin/Info.plist")
	if err != nil {
		t.Fatal(err)
	}
	plist := string(contents)
	if !strings.Contains(plist, "<key>CFBundleDisplayName</key>\n        <string>Cineko</string>") {
		t.Fatal("macOS Launcher does not own the Cineko application identity")
	}
	if strings.Contains(plist, "<key>LSUIElement</key>") {
		t.Fatal("macOS Launcher must remain the user-facing Dock application")
	}
}

func TestResolvedReleaseBaseURLPrefersEnvironmentOverride(t *testing.T) {
	previous := launcherReleaseBaseURL
	launcherReleaseBaseURL = "https://embedded.example"
	t.Cleanup(func() { launcherReleaseBaseURL = previous })
	t.Setenv("CINEKO_RELEASE_BASE_URL", " https://override.example ")
	if got := resolvedReleaseBaseURL(); got != "https://override.example" {
		t.Fatalf("resolved release base URL = %q", got)
	}
	t.Setenv("CINEKO_RELEASE_BASE_URL", "")
	if got := resolvedReleaseBaseURL(); got != "https://embedded.example" {
		t.Fatalf("embedded release base URL = %q", got)
	}
}

func TestWailsAssetServerStaticMiddlewareLogsRequest(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	handler, err := wailsassetserver.NewAssetHandler(assetoptions.Options{
		Assets:     launcher.Assets(),
		Middleware: telemetry.HTTPServerMiddleware(logger),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://launcher.local/launcher.css", nil)
	request.Header.Set("X-Request-Id", "asset-request")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.Len() == 0 {
		t.Fatalf("asset response = status %d, bytes %d", response.Code, response.Body.Len())
	}
	if response.Header().Get("X-Request-Id") != "asset-request" {
		t.Fatalf("asset response X-Request-Id = %q", response.Header().Get("X-Request-Id"))
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["service"] != "launcher" || record["event"] != "http.server.request.completed" ||
		record["request_id"] != "asset-request" || record["method"] != "GET" ||
		record["path"] != "/launcher.css" || record["status"] != float64(http.StatusOK) {
		t.Fatalf("asset HTTP log = %#v", record)
	}
}
