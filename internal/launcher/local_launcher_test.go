package launcher

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	releasepb "github.com/cineko-org/contracts/v3/gen/go/cineko/release"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestRunDirectClient(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is covered on macOS and Linux runners")
	}
	dataDir := t.TempDir()
	clientPath := filepath.Join(t.TempDir(), "cineko-client")
	script := `#!/bin/sh
payload=$(cat)
nonce="$CINEKO_STARTUP_READY_NONCE"
[ -n "$nonce" ] || exit 23
mkdir -p "$CINEKO_DATA_DIR/runtime/startup"
printf '%s\n' "$nonce" > "$CINEKO_DATA_DIR/runtime/startup/$nonce.ready"
chmod 600 "$CINEKO_DATA_DIR/runtime/startup/$nonce.ready"
printf '%s' "$payload"
`
	if err := os.WriteFile(clientPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(clientPath, 0o700); err != nil { // #nosec G302 -- executable test fixture.
		t.Fatal(err)
	}
	var output bytes.Buffer
	started := 0
	err := Run(t.Context(), Config{
		ClientPath: clientPath, DataDir: dataDir, Version: "1.0.0",
		Stdout: &output, Stderr: &output, OnClientStarted: func() { started++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if started != 1 || !strings.Contains(output.String(), `"installationId":"install_`) ||
		!strings.Contains(output.String(), `"clientVersion":"dev"`) || strings.Contains(output.String(), "launchTicket") {
		t.Fatalf("direct Client output = %q, starts = %d", output.String(), started)
	}
	info, err := os.Stat(filepath.Join(dataDir, "installation.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("identity permissions = %v, %v", info, err)
	}
}

func TestFetchReleaseProtoUsesPublicPlatformPathAndLogs(t *testing.T) {
	channel, platform, architecture, version := "stable", runtime.GOOS, runtime.GOARCH, "1.2.3"
	release := releasepb.LauncherRelease_builder{
		Channel: &channel, Platform: &platform, Architecture: &architecture, Version: &version,
	}.Build()
	contents, err := protojson.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	requestedPath := ""
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestedPath = request.URL.Path
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(contents)
	}))
	t.Cleanup(server.Close)
	var logs bytes.Buffer
	config := Config{ReleaseBaseURL: server.URL + "/releases", HTTPClient: server.Client(), Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	output := &releasepb.LauncherRelease{}
	if err := fetchReleaseProto(t.Context(), config, "launcher.json", output); err != nil {
		t.Fatal(err)
	}
	wantPath := "/releases/" + runtime.GOOS + "-" + runtime.GOARCH + "/launcher.json"
	if requestedPath != wantPath || output.GetVersion() != version {
		t.Fatalf("request path/version = %q/%q, want %q/%q", requestedPath, output.GetVersion(), wantPath, version)
	}
	if !strings.Contains(logs.String(), `"event":"http.client.request.completed"`) || !strings.Contains(logs.String(), `"path":"`+wantPath+`"`) {
		t.Fatalf("release HTTP log = %s", logs.String())
	}
}

func TestReleaseBaseURLRejectsNonLoopbackHTTP(t *testing.T) {
	if err := validateReleaseBaseURL("http://releases.example/cineko"); err == nil {
		t.Fatal("non-loopback HTTP release URL accepted")
	}
	if err := validateReleaseBaseURL("http://127.0.0.1:8080/cineko"); err != nil {
		t.Fatalf("loopback release URL rejected: %v", err)
	}
}
