//go:build !darwin || !cgo

package desktop

var platformFocusClient func(int) error

func installActivationHandler(func()) {}
func removeActivationHandler()        {}
