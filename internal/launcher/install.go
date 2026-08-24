package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	releasepb "github.com/cineko-org/contracts/v3/gen/go/cineko/release"
	"github.com/cineko-org/launcher/internal/launcher/artifact"
	"github.com/cineko-org/launcher/internal/launcher/managedfiles"
	installedruntime "github.com/cineko-org/launcher/internal/launcher/runtime"

	"google.golang.org/protobuf/encoding/protojson"
)

func installRelease(
	ctx context.Context,
	config Config,
	release *releasepb.RuntimeRelease,
) (installedRelease, error) {
	manifestPath := filepath.Join(config.DataDir, "runtime", "installed.json")
	previousPath := filepath.Join(config.DataDir, "runtime", "previous.json")
	if installed, err := loadInstalledRelease(config.DataDir, manifestPath, release); err == nil {
		if previous, err := loadInstalledManifest(config.DataDir, previousPath); err == nil {
			installed.Previous = &previous
		}
		report(config, Progress{Stage: StageInstalling, Message: "설치된 업데이트 검증 완료"})
		return installed, nil
	}
	previous, previousErr := loadInstalledManifest(config.DataDir, manifestPath)
	if retained, err := loadInstalledManifest(config.DataDir, previousPath); err == nil {
		previous, previousErr = retained, nil
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: artifact.DefaultDownloadTimeout}
	}
	paths, err := installArtifacts(ctx, config, client, release)
	if err != nil {
		return installedRelease{}, err
	}
	installed := installedRelease{
		Release: release, ClientPath: paths["client"], BrowserPath: paths["browser"],
		DriverPath: paths["playwright"],
	}
	if previousErr == nil && !sameRuntimeRelease(previous.Release, installed.Release) {
		if err := managedfiles.WriteJSONAtomic(previousPath, previous); err != nil {
			return installedRelease{}, err
		}
		installed.Previous = &previous
	}
	if err := managedfiles.WriteJSONAtomic(manifestPath, installed); err != nil {
		return installedRelease{}, err
	}
	report(config, Progress{Stage: StageInstalling, Message: "업데이트 설치 완료"})
	return installed, nil
}

func installArtifacts(
	ctx context.Context,
	config Config,
	client *http.Client,
	release *releasepb.RuntimeRelease,
) (map[string]string, error) {
	items := []installedruntime.Component{
		{Name: "client", Artifact: release.GetClient().GetArtifact()},
		{Name: "browser", Artifact: release.GetBrowser().GetArtifact()},
		{Name: "playwright", Artifact: release.GetPlaywright().GetArtifact()},
	}
	total := release.GetClient().GetArtifact().GetSize() + release.GetBrowser().GetArtifact().GetSize() + release.GetPlaywright().GetArtifact().GetSize()
	paths := make(map[string]string, len(items))
	var completed int64
	for _, item := range items {
		destination := filepath.Join(config.DataDir, "components", item.Name, item.Artifact.GetSha256())
		if executable, err := installedruntime.LoadComponent(destination, item); err == nil {
			paths[item.Name] = executable
			completed += item.Artifact.GetSize()
			report(config, Progress{
				Stage: StageChecking, Message: "설치된 구성요소 확인 중", Artifact: item.Name,
				Downloaded: completed, Total: total,
			})
			continue
		}
		report(config, Progress{
			Stage: StageDownloading, Message: "업데이트 다운로드 중", Artifact: item.Name,
			Downloaded: completed, Total: total,
		})
		archivePath, err := artifact.Download(
			ctx, client, filepath.Join(config.DataDir, "downloads"), item.Name, item.Artifact,
			func(downloaded int64) {
				report(config, Progress{
					Stage: StageDownloading, Message: "업데이트 다운로드 중", Artifact: item.Name,
					Downloaded: completed + downloaded, Total: total,
				})
			},
		)
		if err != nil {
			return nil, err
		}
		report(config, Progress{Stage: StageInstalling, Message: "다운로드 검증 및 설치 중", Artifact: item.Name})
		executable, err := installedruntime.ActivateComponent(archivePath, destination, item)
		if err != nil {
			return nil, fmt.Errorf("install %s artifact: %w", item.Name, err)
		}
		paths[item.Name] = executable
		completed += item.Artifact.GetSize()
		_ = os.Remove(archivePath)
	}
	return paths, nil
}

// MarshalJSON keeps the launcher-only installation metadata alongside the
// generated RuntimeRelease ProtoJSON representation. The release itself is
// never converted to a copied DTO or encoded with encoding/json.
func (installed installedRelease) MarshalJSON() ([]byte, error) {
	if installed.Release == nil {
		return nil, errors.New("installed runtime release is missing")
	}
	release, err := (protojson.MarshalOptions{UseProtoNames: false}).Marshal(installed.Release)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"release":     json.RawMessage(release),
		"clientPath":  installed.ClientPath,
		"browserPath": installed.BrowserPath,
		"driverPath":  installed.DriverPath,
	})
}

func (installed *installedRelease) UnmarshalJSON(contents []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(contents, &fields); err != nil {
		return err
	}
	for key := range fields {
		switch key {
		case "release", "clientPath", "browserPath", "driverPath":
		case "probePublicKeyHash", "probePublicKeySpec":
			// Ignored while reading an older installed manifest.
		default:
			return fmt.Errorf("unknown installed runtime manifest field %q", key)
		}
	}
	releaseContents, ok := fields["release"]
	if !ok {
		return errors.New("installed runtime release is missing")
	}
	release := &releasepb.RuntimeRelease{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(releaseContents, release); err != nil {
		return err
	}
	decodeString := func(name string) (string, error) {
		contents, ok := fields[name]
		if !ok {
			return "", fmt.Errorf("installed runtime manifest field %q is missing", name)
		}
		var value string
		if err := json.Unmarshal(contents, &value); err != nil {
			return "", err
		}
		return value, nil
	}
	clientPath, err := decodeString("clientPath")
	if err != nil {
		return err
	}
	browserPath, err := decodeString("browserPath")
	if err != nil {
		return err
	}
	driverPath, err := decodeString("driverPath")
	if err != nil {
		return err
	}
	*installed = installedRelease{
		Release: release, ClientPath: clientPath, BrowserPath: browserPath, DriverPath: driverPath,
	}
	return nil
}

func loadInstalledRelease(
	dataDir string,
	manifestPath string,
	release *releasepb.RuntimeRelease,
) (installedRelease, error) {
	installed, err := loadInstalledManifest(dataDir, manifestPath)
	if err != nil {
		return installedRelease{}, err
	}
	if !sameRuntimeRelease(installed.Release, release) {
		return installedRelease{}, errors.New("installed runtime manifest does not match release")
	}
	return installed, nil
}

func loadInstalledManifest(dataDir string, manifestPath string) (installedRelease, error) {
	contents, err := os.ReadFile(manifestPath) // #nosec G304 -- scoped runtime manifest.
	if err != nil {
		return installedRelease{}, err
	}
	var installed installedRelease
	if err := json.Unmarshal(contents, &installed); err != nil {
		return installedRelease{}, errors.New("installed runtime manifest is invalid")
	}
	if err := validateInstalledExecutables(dataDir, installed); err != nil {
		return installedRelease{}, err
	}
	return installed, nil
}

func sameRuntimeRelease(left *releasepb.RuntimeRelease, right *releasepb.RuntimeRelease) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.GetClient().GetVersion() == right.GetClient().GetVersion() &&
		left.GetClient().GetArtifact().GetSha256() == right.GetClient().GetArtifact().GetSha256() &&
		left.GetBrowser().GetRevision() == right.GetBrowser().GetRevision() &&
		left.GetBrowser().GetArtifact().GetSha256() == right.GetBrowser().GetArtifact().GetSha256() &&
		left.GetPlaywright().GetVersion() == right.GetPlaywright().GetVersion() &&
		left.GetPlaywright().GetArtifact().GetSha256() == right.GetPlaywright().GetArtifact().GetSha256()
}

func rollbackInstalledRelease(dataDir string, installed installedRelease) error {
	if installed.Previous == nil {
		return nil
	}
	if err := validateInstalledExecutables(dataDir, *installed.Previous); err != nil {
		return err
	}
	if err := managedfiles.WriteJSONAtomic(filepath.Join(dataDir, "runtime", "installed.json"), installed.Previous); err != nil {
		return err
	}
	return os.Remove(filepath.Join(dataDir, "runtime", "previous.json"))
}

func finalizeInstalledRelease(dataDir string, installed installedRelease) {
	if err := os.Remove(filepath.Join(dataDir, "runtime", "previous.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
	installedruntime.Cleanup(dataDir, map[string]string{
		"client": installed.Release.GetClient().GetArtifact().GetSha256(), "browser": installed.Release.GetBrowser().GetArtifact().GetSha256(),
		"playwright": installed.Release.GetPlaywright().GetArtifact().GetSha256(),
	})
}

func validateInstalledExecutables(dataDir string, installed installedRelease) error {
	componentsRoot := filepath.Join(dataDir, "components")
	items := []struct {
		name       string
		artifact   *releasepb.Artifact
		storedPath string
	}{
		{name: "client", artifact: installed.Release.GetClient().GetArtifact(), storedPath: installed.ClientPath},
		{name: "browser", artifact: installed.Release.GetBrowser().GetArtifact(), storedPath: installed.BrowserPath},
		{name: "playwright", artifact: installed.Release.GetPlaywright().GetArtifact(), storedPath: installed.DriverPath},
	}
	for _, item := range items {
		root := filepath.Join(componentsRoot, item.name, item.artifact.GetSha256())
		executable, err := installedruntime.LoadComponent(root, installedruntime.Component{Name: item.name, Artifact: item.artifact})
		if err != nil {
			return fmt.Errorf("validate installed %s: %w", item.name, err)
		}
		if filepath.Clean(item.storedPath) != executable {
			return fmt.Errorf("installed %s path does not match its content-addressed component", item.name)
		}
	}
	return nil
}
