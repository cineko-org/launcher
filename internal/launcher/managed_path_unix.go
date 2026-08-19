//go:build !windows

package launcher

func pathHasReparsePoint(string) (bool, error) { return false, nil }
