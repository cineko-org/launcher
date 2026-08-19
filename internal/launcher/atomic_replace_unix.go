//go:build !windows

package launcher

import "os"

func replaceFileAtomic(source string, destination string) error {
	return os.Rename(source, destination)
}
