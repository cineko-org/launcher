package launcher

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	releasepb "github.com/cineko-org/contracts/gen/go/cineko/release"
	"github.com/cineko-org/launcher/internal/launcher/managedfiles"
	installedruntime "github.com/cineko-org/launcher/internal/launcher/runtime"
	"google.golang.org/protobuf/proto"
)

func TestProbePublicKeyIDsCannotEscapeInstallDirectory(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	root := filepath.Join(t.TempDir(), "components", "keyring")
	for _, keyID := range []string{"../outside", "nested/key", ".", ""} {
		if _, _, err := installProbePublicKeys(root, map[string]string{keyID: publicKey}); err == nil {
			t.Fatalf("unsafe key ID %q accepted", keyID)
		}
	}
	digest, spec, err := installProbePublicKeys(root, map[string]string{"primary-2026_08": publicKey})
	if err != nil {
		t.Fatalf("valid key ID rejected: %v", err)
	}
	if !strings.Contains(spec, filepath.Join("keyring", digest, "primary-2026_08.pem")) {
		t.Fatalf("keyring spec = %q", spec)
	}
}

func TestInstalledManifestRejectsMutatedComponentTree(t *testing.T) {
	dataDir := t.TempDir()
	installed := installedFixture(t, dataDir, "a")
	manifest := filepath.Join(dataDir, "runtime", "installed.json")
	if err := managedfiles.WriteJSONAtomic(manifest, installed); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInstalledManifest(dataDir, manifest); err != nil {
		t.Fatalf("valid installed manifest rejected: %v", err)
	}
	writeExecutableFixture(t, installed.BrowserPath, []byte("tampered"))
	if _, err := loadInstalledManifest(dataDir, manifest); err == nil {
		t.Fatal("installed manifest accepted a mutated browser component")
	}
}

func TestRuntimeReleaseCacheIncludesCompleteLaunchIdentity(t *testing.T) {
	base := installedFixture(t, t.TempDir(), "a").Release
	for name, mutate := range map[string]func(*releasepb.RuntimeRelease){
		"client version": func(release *releasepb.RuntimeRelease) { release.GetClient().SetVersion("2.0.0") },
		"client artifact": func(release *releasepb.RuntimeRelease) {
			release.GetClient().GetArtifact().SetSha256(strings.Repeat("b", 64))
		},
		"browser revision":   func(release *releasepb.RuntimeRelease) { release.GetBrowser().SetRevision("2") },
		"Playwright version": func(release *releasepb.RuntimeRelease) { release.GetPlaywright().SetVersion("2.0.0") },
		"Probe keyring": func(release *releasepb.RuntimeRelease) {
			release.GetClient().SetProbeBootstrapPublicKeys(map[string]string{"rotated": testPublicKeyPEM(t)})
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := proto.CloneOf(base)
			mutate(changed)
			if sameRuntimeRelease(base, changed) {
				t.Fatal("changed launch identity reused the installed runtime")
			}
		})
	}
}

func TestRuntimeRollbackRestoresPreviousManifest(t *testing.T) {
	dataDir := t.TempDir()
	previous := installedFixture(t, dataDir, "1")
	current := installedFixture(t, dataDir, "2")
	current.Previous = &previous
	runtimeRoot := filepath.Join(dataDir, "runtime")
	if err := managedfiles.WriteJSONAtomic(filepath.Join(runtimeRoot, "installed.json"), current); err != nil {
		t.Fatal(err)
	}
	if err := managedfiles.WriteJSONAtomic(filepath.Join(runtimeRoot, "previous.json"), previous); err != nil {
		t.Fatal(err)
	}
	if err := rollbackInstalledRelease(dataDir, current); err != nil {
		t.Fatal(err)
	}
	restored, err := loadInstalledManifest(dataDir, filepath.Join(runtimeRoot, "installed.json"))
	if err != nil || !sameRuntimeRelease(restored.Release, previous.Release) {
		t.Fatalf("restored runtime = %+v, error = %v", restored.Release, err)
	}
	if _, err := os.Stat(filepath.Join(runtimeRoot, "previous.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previous slot was not consumed: %v", err)
	}
	if _, err := os.Stat(current.ClientPath); err != nil {
		t.Fatalf("failed runtime was deleted before rollback completed: %v", err)
	}
}

func installedFixture(t *testing.T, dataDir string, suffix string) installedRelease {
	t.Helper()
	digest := strings.Repeat(suffix, 64)
	paths := map[string]string{}
	for _, component := range []string{"client", "browser", "playwright"} {
		root := filepath.Join(dataDir, "components", component, digest)
		var files map[string]string
		if component == "playwright" {
			files = map[string]string{"node": "fixture", "package/cli.js": "fixture"}
		} else {
			files = map[string]string{component: "fixture"}
		}
		archivePath := filepath.Join(dataDir, "fixtures", component+"-"+suffix+".zip")
		if err := os.MkdirAll(filepath.Dir(archivePath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(archivePath, zipArtifacts(t, files), 0o600); err != nil {
			t.Fatal(err)
		}
		executable := component
		if component == "playwright" {
			executable = "node"
		}
		path, err := installedruntime.ActivateComponent(archivePath, root, installedruntime.Component{
			Name:     component,
			Artifact: testArtifact("https://fixtures.example/"+component+".zip", 0, digest, executable),
		})
		if err != nil {
			t.Fatal(err)
		}
		paths[component] = path
	}
	keyring := map[string]string{"primary": testPublicKeyPEM(t)}
	keyHash, keySpec, err := installProbePublicKeys(filepath.Join(dataDir, "components", "keyring"), keyring)
	if err != nil {
		t.Fatal(err)
	}
	return installedRelease{
		Release: runtimeRelease(
			func() *releasepb.ClientRelease {
				release := &releasepb.ClientRelease{}
				release.SetArtifact(testArtifact("", 0, digest, "client"))
				release.SetProbeBootstrapPublicKeys(keyring)
				return release
			}(),
			func() *releasepb.BrowserRelease {
				release := &releasepb.BrowserRelease{}
				release.SetArtifact(testArtifact("", 0, digest, "browser"))
				return release
			}(),
			func() *releasepb.PlaywrightRelease {
				release := &releasepb.PlaywrightRelease{}
				release.SetArtifact(testArtifact("", 0, digest, "node"))
				return release
			}(),
		),
		ClientPath: paths["client"], BrowserPath: paths["browser"], DriverPath: paths["playwright"],
		ProbePublicKeyHash: keyHash, ProbePublicKeySpec: keySpec,
	}
}

func writeExecutableFixture(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil { // #nosec G302 -- executable test fixture.
		t.Fatal(err)
	}
}
