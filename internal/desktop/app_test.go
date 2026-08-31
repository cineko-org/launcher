package desktop

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	releasepb "github.com/cineko-org/contracts/v3/gen/go/cineko/release"
	"github.com/cineko-org/launcher/internal/launcher"
	"github.com/cineko-org/launcher/internal/telemetry"
)

func TestLauncherInitialState(t *testing.T) {
	app := New(launcher.Config{Version: "1.2.3"}, nil)
	state := app.State()
	if state.Mode != ModeChecking || state.Version != "1.2.3" || state.Revision != 1 {
		t.Fatalf("initial state = %+v", state)
	}
}

func TestLauncherStateRevisionIsMonotonic(t *testing.T) {
	app := New(launcher.Config{Version: "1.2.3"}, nil)
	previous := app.State().Revision
	for _, mode := range []Mode{ModeUpdating, ModeLaunching, ModeError} {
		app.publish(State{Mode: mode, Message: "state", Version: "1.2.3"})
		current := app.State().Revision
		if current != previous+1 {
			t.Fatalf("state revision %d followed %d", current, previous)
		}
		previous = current
	}
}

func TestLauncherPublishesPortableUpdate(t *testing.T) {
	app := New(launcher.Config{Version: "1.2.3"}, nil)
	artifact := releasepb.Artifact_builder{Url: stringPointer("https://releases.example/launcher.zip")}.Build()
	app.publishFailure(launcher.Config{Version: "1.2.3"}, &launcher.LauncherUpdateRequired{
		Version: "1.3.0", Artifact: artifact,
	})
	state := app.State()
	if state.Mode != ModeLauncherUpdate || state.LatestVersion != "1.3.0" ||
		state.DownloadURL != "https://releases.example/launcher.zip" {
		t.Fatalf("Launcher update state = %+v", state)
	}
}

func TestLauncherProgressMapsToDesktopState(t *testing.T) {
	app := New(launcher.Config{Version: "1.2.3"}, nil)
	app.progress(launcher.Progress{Stage: launcher.StageDownloading, Message: "다운로드 중", Artifact: "client", Downloaded: 50, Total: 100})
	state := app.State()
	if state.Mode != ModeUpdating || state.Artifact != "client" || state.Downloaded != 50 || state.Total != 100 {
		t.Fatalf("download state = %+v", state)
	}
	app.progress(launcher.Progress{Stage: launcher.StageLaunching, Message: "시작 중"})
	if state = app.State(); state.Mode != ModeLaunching || state.Stage != launcher.StageLaunching {
		t.Fatalf("launch state = %+v", state)
	}
}

func TestUserFacingErrorDoesNotExposeInternalDetail(t *testing.T) {
	message := userFacingError(errors.New(`Get "https://releases.internal/runtime.json": dial tcp: no such host`))
	if message != "업데이트 서버에 연결할 수 없습니다. 네트워크 연결을 확인한 뒤 다시 시도하세요." {
		t.Fatalf("connection message = %q", message)
	}
	message = userFacingError(errors.New("verify client artifact: SHA-256 mismatch"))
	if message != "업데이트 파일을 받지 못했습니다. 네트워크 연결을 확인한 뒤 다시 시도하세요." {
		t.Fatalf("artifact message = %q", message)
	}
}

func TestDownloadLauncherContextUsesAppLogger(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	app := New(launcher.Config{Version: "1.2.3", Logger: logger}, nil)
	request := &http.Request{Method: http.MethodGet, Header: make(http.Header), URL: &url.URL{Path: "/launcher.zip"}}
	telemetry.LogHTTPClientRequest(app.requestContext(context.Background(), app.config), request, &http.Response{StatusCode: http.StatusOK}, time.Now(), 0, 0, nil)
	if !strings.Contains(output.String(), `"event":"http.client.request.completed"`) || !strings.Contains(output.String(), `"service":"launcher"`) {
		t.Fatalf("DownloadLauncher context did not use app logger: %s", output.String())
	}
}

func stringPointer(value string) *string { return &value }
