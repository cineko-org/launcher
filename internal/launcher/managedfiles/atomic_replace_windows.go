//go:build windows

package managedfiles

import "golang.org/x/sys/windows"

// ReplaceAtomic replaces destination with source using the platform's atomic operation.
func ReplaceAtomic(source string, destination string) error {
	sourcePointer, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPointer, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		sourcePointer,
		destinationPointer,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}
