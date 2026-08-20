package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	clientStartupTimeout = 30 * time.Second
	startupCheckInterval = 25 * time.Millisecond
)

func startupReadyPath(dataDir, nonce string) (string, error) {
	if len(nonce) < 16 || len(nonce) > 128 {
		return "", errors.New("startup nonce is invalid")
	}
	for _, character := range nonce {
		if character != '-' && character != '_' && (character < '0' || character > '9') &&
			(character < 'A' || character > 'Z') && (character < 'a' || character > 'z') {
			return "", errors.New("startup nonce is invalid")
		}
	}
	return filepath.Join(dataDir, "runtime", "startup", nonce+".ready"), nil
}

func prepareStartupReady(dataDir, nonce string) (string, error) {
	path, err := startupReadyPath(dataDir, nonce)
	if err != nil {
		return "", err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create startup directory: %w", err)
	}
	for _, candidate := range []string{dataDir, filepath.Join(dataDir, "runtime"), directory} {
		if err := rejectSymlink(candidate); err != nil {
			return "", err
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(directory, 0o700); err != nil {
			return "", fmt.Errorf("secure startup directory: %w", err)
		}
	}
	if _, err := os.Lstat(path); err == nil {
		return "", errors.New("stale startup marker exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect startup marker: %w", err)
	}
	return path, nil
}

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect startup directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("startup directory is not a private directory")
	}
	return nil
}

func awaitStartupReady(
	ctx context.Context,
	path string,
	nonce string,
	processDone chan error,
	timeout time.Duration,
	interval time.Duration,
) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-processDone:
			if ready, markerErr := consumeStartupMarker(path, nonce); markerErr != nil {
				return markerErr
			} else if ready {
				processDone <- err
				return nil
			}
			if err == nil {
				return errors.New("Cineko Client exited before startup was ready")
			}
			return err
		case <-timer.C:
			return errors.New("Cineko Client startup timed out")
		case <-ticker.C:
			ready, err := consumeStartupMarker(path, nonce)
			if err != nil {
				return err
			}
			if ready {
				return nil
			}
		}
	}
}

func consumeStartupMarker(path, nonce string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		(runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return false, errors.New("Client startup marker is invalid")
	}
	contents, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(contents)) != nonce {
		return false, errors.New("Client startup marker is invalid")
	}
	if err := os.Remove(path); err != nil {
		return false, fmt.Errorf("remove Client startup marker: %w", err)
	}
	return true, nil
}
