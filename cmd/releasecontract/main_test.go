package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	commonpb "github.com/cineko-org/contracts/v3/gen/go/cineko/common"
	releasepb "github.com/cineko-org/contracts/v3/gen/go/cineko/release"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestGeneratedLauncherReleaseSet(t *testing.T) {
	paths, payload := testReleaseSet(t)
	if len(paths) != 3 {
		t.Fatalf("release paths = %d", len(paths))
	}
	if !regexp.MustCompile(`"size":\s*"[1-9][0-9]*"`).Match(payload) {
		t.Fatalf("ProtoJSON int64 size was not encoded as a string: %s", payload)
	}

	set := &releasepb.LauncherReleaseSet{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(payload, set); err != nil {
		t.Fatal(err)
	}
	if len(set.GetReleases()) != 3 || releaseKey(set.GetReleases()[0]) != "darwin/arm64" || releaseKey(set.GetReleases()[2]) != "windows/amd64" {
		t.Fatalf("generated release set = %+v", set.GetReleases())
	}

	if _, err := readReleaseSet(paths[:2]); err == nil {
		t.Fatal("incomplete Launcher release set accepted")
	}
	if err := writeRelease(io.Discard, []string{
		"latest", "darwin/arm64", strings.TrimSuffix(paths[0], ".json"),
		"Cineko Launcher.app/Contents/MacOS/Cineko Launcher",
		"https://github.example/releases/darwin-arm64.zip", "2026-08-12T00:00:00Z",
	}); err == nil {
		t.Fatal("non-semantic Launcher version accepted")
	}
}

func TestPublishLauncherRelease(t *testing.T) {
	_, payload := testReleaseSet(t)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer publisher" || request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request headers = %v", request.Header)
		}
		set := &releasepb.LauncherReleaseSet{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(readBody(t, request), set); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("X-Cineko-Release-Generation", "23")
		writer.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	if err := publishLauncherRelease(t.Context(), server.Client(), func(time.Duration) {}, server.URL, "publisher", payload); err != nil {
		t.Fatal(err)
	}
}

func TestPublishLauncherReleaseRetriesOnlyServerFailures(t *testing.T) {
	_, payload := testReleaseSet(t)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("X-Cineko-Release-Generation", "24")
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	if err := publishLauncherRelease(t.Context(), server.Client(), func(time.Duration) {}, server.URL, "publisher", payload); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("server attempts = %d", attempts.Load())
	}

	var conflictAttempts atomic.Int32
	conflictServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		conflictAttempts.Add(1)
		code, message, requestID := "release_conflict", "immutable release changed", "request-1"
		retryable := false
		failure := commonpb.APIErrorResponse_builder{Error: commonpb.APIError_builder{
			Code: &code, Message: &message, Retryable: &retryable, RequestId: &requestID,
		}.Build()}.Build()
		body, err := protojson.Marshal(failure)
		if err != nil {
			t.Error(err)
		}
		writer.WriteHeader(http.StatusConflict)
		_, _ = writer.Write(body)
	}))
	defer conflictServer.Close()
	if err := publishLauncherRelease(t.Context(), conflictServer.Client(), func(time.Duration) {}, conflictServer.URL, "publisher", payload); err == nil || !strings.Contains(err.Error(), "release_conflict") {
		t.Fatalf("conflict error = %v", err)
	}
	if conflictAttempts.Load() != 1 {
		t.Fatalf("conflict attempts = %d", conflictAttempts.Load())
	}
}

func TestPublishResponseAndGenerationContract(t *testing.T) {
	if err := validatePublishResponse(nil); err != nil {
		t.Fatalf("empty generated response = %v", err)
	}
	if err := validatePublishResponse([]byte(`{"generation":"42"}`)); err == nil {
		t.Fatal("out-of-contract response field accepted")
	}
	if generation, err := positiveGeneration("31"); err != nil || generation != 31 {
		t.Fatalf("generation = %d, %v", generation, err)
	}
	for _, value := range []string{"", "0", "-1", "invalid"} {
		if _, err := positiveGeneration(value); err == nil {
			t.Fatalf("invalid generation %q accepted", value)
		}
	}
}

func TestPublishCommandKeepsTokenOutOfArguments(t *testing.T) {
	paths, _ := testReleaseSet(t)
	var set bytes.Buffer
	if err := writeSet(&set, paths); err != nil {
		t.Fatal(err)
	}
	setPath := filepath.Join(t.TempDir(), "launcher-set.json")
	if err := os.WriteFile(setPath, set.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CINEKO_RELEASE_PUBLISH_TOKEN", "")
	if err := publishFromArgs([]string{"https://central.example", setPath}); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("missing environment token error = %v", err)
	}
	if err := publishFromArgs([]string{"https://central.example", "secret-on-command-line", setPath}); err == nil || !strings.Contains(err.Error(), usage) {
		t.Fatalf("command-line token was accepted: %v", err)
	}
}

func testReleaseSet(t *testing.T) ([]string, []byte) {
	t.Helper()
	root := t.TempDir()
	paths := make([]string, 0, 3)
	for _, target := range []struct {
		platform   string
		extension  string
		executable string
	}{
		{"darwin/arm64", "zip", "Cineko Launcher.app/Contents/MacOS/Cineko Launcher"},
		{"linux/amd64", "AppImage", "cineko-launcher-v1.2.3-linux-amd64.AppImage"},
		{"windows/amd64", "exe", "Cineko Launcher.exe"},
	} {
		artifact := filepath.Join(root, strings.ReplaceAll(target.platform, "/", "-")+"."+target.extension)
		if err := os.WriteFile(artifact, []byte("portable Launcher\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var release bytes.Buffer
		if err := writeRelease(&release, []string{
			"1.2.3", target.platform, artifact, target.executable,
			"https://github.example/releases/" + filepath.Base(artifact), "2026-08-12T00:00:00Z",
		}); err != nil {
			t.Fatal(err)
		}
		path := artifact + ".json"
		if err := os.WriteFile(path, release.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	var set bytes.Buffer
	if err := writeSet(&set, paths); err != nil {
		t.Fatal(err)
	}
	return paths, set.Bytes()
}

func readBody(t *testing.T, request *http.Request) []byte {
	t.Helper()
	payload, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
