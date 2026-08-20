//go:build !windows

package managedfiles

import "os"

// ReplaceAtomic replaces destination with source using the platform's atomic operation.
func ReplaceAtomic(source string, destination string) error {
	return os.Rename(source, destination)
}
