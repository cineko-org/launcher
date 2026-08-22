package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	releasepb "github.com/cineko-org/contracts/v3/gen/go/cineko/release"
)

func TestDefaultDownloadTimeoutIsTenMinutes(t *testing.T) {
	if DefaultDownloadTimeout != 10*time.Minute {
		t.Fatalf("DefaultDownloadTimeout = %s, want 10m", DefaultDownloadTimeout)
	}
}

func TestDownloadResumesAndReusesVerifiedCache(t *testing.T) {
	payload := bytes.Repeat([]byte("cineko-range-fixture"), 64*1024)
	offset := int64(len(payload) / 2)
	var requests atomic.Int32
	var transferred atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Header.Get("Authorization") != "" {
			t.Error("public CDN request included Central credentials")
		}
		if got := request.Header.Get("Range"); got != fmt.Sprintf("bytes=%d-", offset) {
			t.Errorf("Range=%q", got)
		}
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(payload)-1, len(payload)))
		writer.WriteHeader(http.StatusPartialContent)
		written, _ := writer.Write(payload[offset:])
		transferred.Add(int64(written))
	}))
	t.Cleanup(server.Close)
	digest := sha256.Sum256(payload)
	artifact := testArtifact(server.URL+"/cineko-client.zip", int64(len(payload)), hex.EncodeToString(digest[:]), "cineko-client")
	cache := t.TempDir()
	partial := filepath.Join(cache, artifact.GetSha256()+".blob.part")
	if err := os.WriteFile(partial, payload[:offset], 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := Download(t.Context(), server.Client(), cache, "client", artifact, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !fileMatchesArtifact(path, artifact) {
		t.Fatal("resumed artifact did not pass digest verification")
	}
	if got, want := transferred.Load(), int64(len(payload))-offset; got != want {
		t.Fatalf("transferred=%d, want=%d", got, want)
	}
	if _, err := Download(t.Context(), server.Client(), cache, "client", artifact, nil); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("verified cache made %d network requests", requests.Load())
	}
	baselineRetry := int64(len(payload))
	saved := baselineRetry - transferred.Load()
	t.Logf(
		"Range resume transferred %d bytes instead of %d bytes for the retry (saved %d bytes, %.1f%%)",
		transferred.Load(), baselineRetry, saved, float64(saved)*100/float64(baselineRetry),
	)
}

func TestDownloadRestartsWhenOriginIgnoresRange(t *testing.T) {
	payload := []byte("complete release payload")
	digest := sha256.Sum256(payload)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = writer.Write(payload)
	}))
	t.Cleanup(server.Close)
	artifact := testArtifact(server.URL+"/client.zip", int64(len(payload)), hex.EncodeToString(digest[:]), "client")
	cache := t.TempDir()
	if err := os.WriteFile(filepath.Join(cache, artifact.GetSha256()+".blob.part"), payload[:4], 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := Download(t.Context(), server.Client(), cache, "client", artifact, nil)
	if err != nil || !fileMatchesArtifact(path, artifact) {
		t.Fatalf("restart result path=%q error=%v", path, err)
	}
	if requests.Load() != 2 {
		t.Fatalf("origin fallback requests=%d", requests.Load())
	}
}

func TestDownloadDoesNotFollowPartialSymlink(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("symlink policy is host-dependent on Windows")
	}
	payload := []byte("verified release")
	digest := sha256.Sum256(payload)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(payload)
	}))
	t.Cleanup(server.Close)
	artifact := testArtifact(server.URL+"/client.zip", int64(len(payload)), hex.EncodeToString(digest[:]), "client")
	cache := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("do-not-change"), 0o600); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(cache, artifact.GetSha256()+".blob.part")
	if err := os.Symlink(outside, partial); err != nil {
		t.Fatal(err)
	}
	path, err := Download(t.Context(), server.Client(), cache, "client", artifact, nil)
	if err != nil || !fileMatchesArtifact(path, artifact) {
		t.Fatalf("safe restart path=%q error=%v", path, err)
	}
	contents, err := os.ReadFile(outside) // #nosec G304 -- test-owned path.
	if err != nil || string(contents) != "do-not-change" {
		t.Fatalf("partial symlink target contents=%q error=%v", contents, err)
	}
}

func TestDownloadPortableLauncherVerifiesBeforeAtomicPlacement(t *testing.T) {
	payload := []byte("portable-launcher")
	digest := sha256.Sum256(payload)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(payload)
	}))
	t.Cleanup(server.Close)
	artifact := testArtifact(server.URL+"/cineko-launcher.AppImage", int64(len(payload)), hex.EncodeToString(digest[:]), "cineko-launcher.AppImage")
	destination := filepath.Join(t.TempDir(), "cineko-launcher.AppImage")
	if err := DownloadPortableLauncher(t.Context(), server.Client(), t.TempDir(), artifact, destination, nil); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(destination) // #nosec G304 -- test-owned destination.
	if err != nil || !bytes.Equal(contents, payload) {
		t.Fatalf("portable Launcher contents=%q error=%v", contents, err)
	}
	if os.PathSeparator == '/' {
		info, err := os.Stat(destination)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("AppImage mode=%v error=%v", info, err)
		}
	}
	artifact.SetSha256(strings.Repeat("0", 64))
	badDestination := destination + ".bad"
	if err := DownloadPortableLauncher(t.Context(), server.Client(), t.TempDir(), artifact, badDestination, nil); err == nil {
		t.Fatal("corrupt portable Launcher accepted")
	}
	if _, err := os.Stat(badDestination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unverified Launcher was placed: %v", err)
	}
}

func testArtifact(url string, size int64, sha256 string, executable string) *releasepb.Artifact {
	artifact := &releasepb.Artifact{}
	artifact.SetUrl(url)
	artifact.SetSize(size)
	artifact.SetSha256(sha256)
	artifact.SetExecutable(executable)
	return artifact
}

func TestContentRangeValidationIsExact(t *testing.T) {
	for _, value := range []string{"bytes 4-9/10", " bytes 4-9/10 "} {
		if !validContentRange(value, 4, 10) {
			t.Fatalf("valid Content-Range rejected: %q", value)
		}
	}
	for _, value := range []string{
		"bytes 3-9/10", "bytes 4-8/10", "bytes 4-9/11", "items 4-9/10", "bytes garbage", "bytes 4-x/10",
	} {
		if validContentRange(value, 4, 10) {
			t.Fatalf("invalid Content-Range accepted: %q", value)
		}
	}
}
