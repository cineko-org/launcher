package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"buf.build/go/protovalidate"
	commonpb "github.com/cineko-org/contracts/v3/gen/go/cineko/common"
	releasepb "github.com/cineko-org/contracts/v3/gen/go/cineko/release"
	servicepb "github.com/cineko-org/contracts/v3/gen/go/cineko/service"
	artifactmetadata "github.com/cineko-org/launcher/internal/launcher/artifact"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	usage = `usage:
  releasecontract release VERSION PLATFORM/ARCH ARTIFACT EXECUTABLE PUBLIC_URL PUBLISHED_AT
  releasecontract set RELEASE_JSON...
  releasecontract publish CENTRAL_URL SET_JSON`
	maxPublishAttempts = 4
	maxResponseBytes   = 1 << 20
)

var semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "release":
		return writeRelease(os.Stdout, args[1:])
	case "set":
		return writeSet(os.Stdout, args[1:])
	case "publish":
		return publishFromArgs(args[1:])
	default:
		return errors.New(usage)
	}
}

func writeRelease(destination io.Writer, args []string) error {
	if len(args) != 6 {
		return errors.New(usage)
	}
	version := strings.TrimPrefix(args[0], "v")
	platform, architecture, err := splitPlatform(args[1])
	if err != nil {
		return err
	}
	publishedAt, err := parseTimestamp(args[5])
	if err != nil {
		return err
	}
	artifact, err := releaseArtifact(args[2], args[4], args[3])
	if err != nil {
		return err
	}
	channel := "stable"
	release := releasepb.LauncherRelease_builder{
		Channel: &channel, Platform: &platform, Architecture: &architecture,
		Version: &version, Launcher: artifact, PublishedAt: publishedAt,
	}.Build()
	return marshalValidated(destination, release)
}

func writeSet(destination io.Writer, paths []string) error {
	set, err := readReleaseSet(paths)
	if err != nil {
		return err
	}
	return marshalValidated(destination, set)
}

func publishFromArgs(args []string) error {
	if len(args) != 2 {
		return errors.New(usage)
	}
	centralURL := strings.TrimSuffix(args[0], "/")
	parsed, err := url.ParseRequestURI(centralURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return errors.New("central URL must be HTTPS")
	}
	token := strings.TrimSpace(os.Getenv("CINEKO_RELEASE_PUBLISH_TOKEN"))
	if token == "" {
		return errors.New("release publisher token is required")
	}
	set, err := readSetFile(args[1])
	if err != nil {
		return err
	}
	payload, err := protojson.Marshal(set)
	if err != nil {
		return fmt.Errorf("encode generated Launcher release set: %w", err)
	}
	endpoint := centralURL + "/v1/release-registry/launcher"
	return publishLauncherRelease(context.Background(), &http.Client{Timeout: 30 * time.Second}, time.Sleep, endpoint, token, payload)
}

func publishLauncherRelease(
	ctx context.Context,
	client *http.Client,
	sleep func(time.Duration),
	endpoint string,
	token string,
	payload []byte,
) error {
	for attempt := 1; attempt <= maxPublishAttempts; attempt++ {
		// #nosec G107,G704 -- publishFromArgs validates the operator-supplied Central HTTPS origin.
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("create Launcher release request: %w", err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")

		// #nosec G704 -- the request URL is the validated Central release-registry endpoint.
		response, err := client.Do(request)
		if err != nil {
			if attempt == maxPublishAttempts {
				return fmt.Errorf("central release registration failed after %d network attempts: %w", attempt, err)
			}
			sleep(publishBackoff(attempt))
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
		closeErr := response.Body.Close()
		if readErr != nil {
			return fmt.Errorf("read Central release response: %w", readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close Central release response: %w", closeErr)
		}

		if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
			if err := validatePublishResponse(body); err != nil {
				return err
			}
			generation, err := positiveGeneration(response.Header.Get("X-Cineko-Release-Generation"))
			if err != nil {
				return err
			}
			fmt.Printf("registered Launcher release generation %d\n", generation)
			return nil
		}

		failure := centralFailure(response.StatusCode, body)
		if response.StatusCode < http.StatusInternalServerError || attempt == maxPublishAttempts {
			return failure
		}
		sleep(publishBackoff(attempt))
	}
	return errors.New("central release registration exhausted all attempts")
}

func validatePublishResponse(payload []byte) error {
	if len(bytes.TrimSpace(payload)) == 0 {
		payload = []byte("{}")
	}
	response := servicepb.PublishLauncherResponse_builder{}.Build()
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(payload, response); err != nil {
		return fmt.Errorf("decode generated Launcher publish response: %w", err)
	}
	if err := protovalidate.Validate(response); err != nil {
		return fmt.Errorf("validate generated Launcher publish response: %w", err)
	}
	return nil
}

func centralFailure(status int, payload []byte) error {
	response := commonpb.APIErrorResponse_builder{}.Build()
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(payload, response); err == nil && response.GetError() != nil {
		return fmt.Errorf("central release registration failed with HTTP %d: %s: %s", status, response.GetError().GetCode(), response.GetError().GetMessage())
	}
	return fmt.Errorf("central release registration failed with HTTP %d", status)
}

func positiveGeneration(value string) (int64, error) {
	generation, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || generation <= 0 {
		return 0, errors.New("central returned an invalid release generation header")
	}
	return generation, nil
}

func publishBackoff(attempt int) time.Duration {
	return time.Second * time.Duration(1<<(attempt-1))
}

func readReleaseSet(paths []string) (*releasepb.LauncherReleaseSet, error) {
	if len(paths) == 0 {
		return nil, errors.New(usage)
	}
	releases := make([]*releasepb.LauncherRelease, 0, len(paths))
	for _, path := range paths {
		release := &releasepb.LauncherRelease{}
		if err := readValidated(path, release); err != nil {
			return nil, err
		}
		releases = append(releases, release)
	}
	sort.Slice(releases, func(i, j int) bool { return releaseKey(releases[i]) < releaseKey(releases[j]) })
	set := releasepb.LauncherReleaseSet_builder{Releases: releases}.Build()
	if err := validateReleaseSet(set); err != nil {
		return nil, err
	}
	return set, nil
}

func readSetFile(path string) (*releasepb.LauncherReleaseSet, error) {
	set := &releasepb.LauncherReleaseSet{}
	payload, err := readOperatorFile(path)
	if err != nil {
		return nil, err
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(payload, set); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := validateReleaseSet(set); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return set, nil
}

func readValidated(path string, message proto.Message) error {
	payload, err := readOperatorFile(path)
	if err != nil {
		return err
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(payload, message); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	if err := protovalidate.Validate(message); err != nil {
		return fmt.Errorf("validate %s: %w", path, err)
	}
	if release, ok := message.(*releasepb.LauncherRelease); ok {
		if err := validateRelease(release); err != nil {
			return fmt.Errorf("validate %s: %w", path, err)
		}
	}
	return nil
}

func marshalValidated(destination io.Writer, message proto.Message) error {
	if err := protovalidate.Validate(message); err != nil {
		return fmt.Errorf("validate generated release message: %w", err)
	}
	switch value := message.(type) {
	case *releasepb.LauncherRelease:
		if err := validateRelease(value); err != nil {
			return fmt.Errorf("validate generated release message: %w", err)
		}
	case *releasepb.LauncherReleaseSet:
		if err := validateReleaseSet(value); err != nil {
			return fmt.Errorf("validate generated release message: %w", err)
		}
	}
	payload, err := (protojson.MarshalOptions{Indent: "  "}).Marshal(message)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(destination, "%s\n", payload)
	return err
}

func validateReleaseSet(set *releasepb.LauncherReleaseSet) error {
	if set == nil || len(set.GetReleases()) != 3 {
		return errors.New("launcher release set must contain exactly three platforms")
	}
	want := map[string]bool{"darwin/arm64": true, "linux/amd64": true, "windows/amd64": true}
	seen := make(map[string]bool, len(set.GetReleases()))
	var publishedAt time.Time
	for index, release := range set.GetReleases() {
		if err := validateRelease(release); err != nil {
			return err
		}
		key := releaseKey(release)
		if !want[key] || seen[key] {
			return fmt.Errorf("invalid or duplicate Launcher release platform %q", key)
		}
		seen[key] = true
		current := release.GetPublishedAt().AsTime()
		if index == 0 {
			publishedAt = current
		} else if !current.Equal(publishedAt) {
			return errors.New("launcher release publication timestamps must match")
		}
	}
	return protovalidate.Validate(set)
}

func validateRelease(release *releasepb.LauncherRelease) error {
	if release == nil || release.GetChannel() != "stable" || !semverPattern.MatchString(release.GetVersion()) {
		return errors.New("launcher release identity is incomplete")
	}
	if err := artifactmetadata.ValidateMetadata(release.GetLauncher()); err != nil {
		return fmt.Errorf("launcher release artifact is invalid: %w", err)
	}
	if release.GetPublishedAt() == nil || release.GetPublishedAt().CheckValid() != nil {
		return errors.New("launcher release publication timestamp is required")
	}
	return protovalidate.Validate(release)
}

func releaseArtifact(path string, publicURL string, executable string) (*releasepb.Artifact, error) {
	info, err := os.Stat(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	payload, err := readOperatorFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	size := info.Size()
	return releasepb.Artifact_builder{Url: &publicURL, Size: &size, Sha256: &hash, Executable: &executable}.Build(), nil
}

func readOperatorFile(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("file path is required")
	}
	// #nosec G304,G703 -- this local operator CLI intentionally accepts explicit release files.
	return os.ReadFile(filepath.Clean(path))
}

func releaseKey(release *releasepb.LauncherRelease) string {
	return release.GetPlatform() + "/" + release.GetArchitecture()
}

func splitPlatform(value string) (string, string, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid platform %q", value)
	}
	return parts[0], parts[1], nil
}

func parseTimestamp(value string) (*timestamppb.Timestamp, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, fmt.Errorf("invalid release timestamp: %w", err)
	}
	timestamp := timestamppb.New(parsed)
	return timestamp, timestamp.CheckValid()
}
