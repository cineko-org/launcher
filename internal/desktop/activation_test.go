package desktop

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/cineko-org/launcher/internal/launcher"
)

func TestShowRoutesToOwnedClientInsteadOfLauncher(t *testing.T) {
	app := New(launcher.Config{}, nil)
	var focused []int
	shown := 0
	app.focusClient = func(pid int) error { focused = append(focused, pid); return nil }
	app.showWindow = func(context.Context) { shown++ }
	app.clientStarted(1234)
	app.ctx = t.Context()
	app.Show()
	app.Show()
	if len(focused) != 2 || focused[0] != 1234 || focused[1] != 1234 || shown != 0 {
		t.Fatalf("activation routing: focused=%v launcher windows=%d", focused, shown)
	}
	app.clientStopped()
	app.Show()
	if len(focused) != 2 || shown != 1 {
		t.Fatalf("stopped Client still owns activation: focused=%v launcher windows=%d", focused, shown)
	}
}

func TestFailedClientActivationDoesNotRevealLauncher(t *testing.T) {
	var output bytes.Buffer
	app := New(launcher.Config{}, slog.New(slog.NewJSONHandler(&output, nil)))
	app.focusClient = func(int) error { return errors.New("activation refused") }
	app.showWindow = func(context.Context) { t.Fatal("revealed updater instead of Client") }
	app.clientStarted(1234)
	app.ctx = t.Context()
	app.Show()
	if !strings.Contains(output.String(), `"event":"launcher.client.activation.failed"`) {
		t.Fatalf("activation failure missing from log: %s", output.String())
	}
}

func TestShowBeforeClientReadyAndOnOtherPlatforms(t *testing.T) {
	app := New(launcher.Config{}, nil)
	app.focusClient = nil
	shown := 0
	app.showWindow = func(context.Context) { shown++ }
	app.Show() // Context not ready yet.
	app.ctx = t.Context()
	app.Show()
	if shown != 1 {
		t.Fatalf("launcher show count = %d", shown)
	}
}
