package desktop

import (
	"errors"
	"testing"

	central "github.com/cineko-org/contracts/v3"
	centralstore "github.com/cineko-org/launcher/internal/centralclient"
	"github.com/cineko-org/launcher/internal/launcher"
)

func TestLauncherInitialStateAndPINValidation(t *testing.T) {
	app := New(launcher.Config{Version: "1.2.3"}, nil)
	state := app.State()
	if state.Mode != ModeChecking || state.Version != "1.2.3" || state.Revision != 1 {
		t.Fatalf("initial state = %+v", state)
	}
	for _, pin := range []string{"", "12345", "12345a", "１２３４５６"} {
		if err := app.Connect(pin); !errors.Is(err, launcher.ErrInvalidPIN) {
			t.Fatalf("Connect(%q) error = %v", pin, err)
		}
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

func TestLauncherPublishesTypedAuthenticationFailures(t *testing.T) {
	app := New(launcher.Config{Version: "1.2.3"}, nil)
	config := launcher.Config{Version: "1.2.3"}
	app.publishFailure(config, centralstore.ErrPINInvalid)
	if state := app.State(); state.Mode != ModeLogin || state.Message != "인증 번호가 올바르지 않습니다." {
		t.Fatalf("invalid PIN state = %+v", state)
	}
	app.publishFailure(config, centralstore.ErrServerUnavailable)
	if state := app.State(); state.Mode != ModeError || state.Message != "서버 응답이 없습니다. 잠시 후 다시 시도하세요." {
		t.Fatalf("server unavailable state = %+v", state)
	}
}

func TestLauncherPublishesPortableUpdate(t *testing.T) {
	app := New(launcher.Config{Version: "1.2.3"}, nil)
	app.publishFailure(launcher.Config{Version: "1.2.3"}, &launcher.LauncherUpdateRequired{
		Version: "1.3.0", Artifact: central.ReleaseArtifact{URL: "https://cdn.example/launcher.zip"},
	})
	state := app.State()
	if state.Mode != ModeLauncherUpdate || state.LatestVersion != "1.3.0" ||
		state.DownloadURL != "https://cdn.example/launcher.zip" ||
		state.Message != "계속하려면 새 Launcher를 내려받아 실행하세요." {
		t.Fatalf("Launcher update state = %+v", state)
	}
}

func TestLauncherProgressMapsToDesktopState(t *testing.T) {
	app := New(launcher.Config{Version: "1.2.3"}, nil)
	app.progress(launcher.Progress{
		Stage: launcher.StageDownloading, Message: "다운로드 중", Artifact: "client",
		Downloaded: 50, Total: 100,
	})
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
	internal := `Post "https://central.internal/v1/auth/pin": dial tcp: lookup central.internal: no such host`
	message := userFacingError(errors.New(internal))
	if message != "Cineko 서비스에 연결할 수 없습니다. 네트워크 연결을 확인한 뒤 다시 시도하세요." {
		t.Fatalf("connection message = %q", message)
	}
	message = userFacingError(errors.New("verify client artifact: SHA-256 mismatch"))
	if message != "업데이트 파일을 받지 못했습니다. 네트워크 연결을 확인한 뒤 다시 시도하세요." {
		t.Fatalf("artifact message = %q", message)
	}
	message = userFacingError(errors.New("unexpected internal detail"))
	if message != "Cineko를 시작할 수 없습니다. 잠시 후 다시 시도해 주세요." {
		t.Fatalf("generic message = %q", message)
	}
}
