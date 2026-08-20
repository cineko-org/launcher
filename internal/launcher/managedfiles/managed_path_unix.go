//go:build !windows

package managedfiles

func pathHasReparsePoint(string) (bool, error) { return false, nil }
