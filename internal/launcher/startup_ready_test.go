package launcher

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestStartupReadyHandshakeAcceptsPrivateMatchingMarker(t *testing.T) {
	dataDir := t.TempDir()
	nonce := "abcdefghijklmnopqrstuvwxyz012345"
	path, err := prepareStartupReady(dataDir, nonce)
	if err != nil {
		t.Fatal(err)
	}
	processDone := make(chan error, 1)
	go func() {
		time.Sleep(10 * time.Millisecond)
		if err := os.WriteFile(path, []byte(nonce+"\n"), 0o600); err != nil {
			processDone <- err
		}
	}()
	if err := awaitStartupReady(t.Context(), path, nonce, processDone, time.Second, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("consumed startup marker remains: %v", err)
	}
}

func TestStartupReadyHandshakeRejectsStaleMarker(t *testing.T) {
	dataDir := t.TempDir()
	nonce := "abcdefghijklmnopqrstuvwxyz012345"
	path, err := prepareStartupReady(dataDir, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(nonce), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareStartupReady(dataDir, nonce); err == nil {
		t.Fatal("stale startup marker was accepted")
	}
}

func TestStartupReadyHandshakeFailsOnExitAndTimeout(t *testing.T) {
	nonce := "abcdefghijklmnopqrstuvwxyz012345"
	for name, setup := range map[string]func(chan error){
		"exit":    func(done chan error) { done <- errors.New("client failed") },
		"timeout": func(chan error) {},
	} {
		t.Run(name, func(t *testing.T) {
			path, err := prepareStartupReady(t.TempDir(), nonce)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			setup(done)
			if err := awaitStartupReady(context.Background(), path, nonce, done, 10*time.Millisecond, time.Millisecond); err == nil {
				t.Fatal("startup failure was accepted")
			}
		})
	}
}
