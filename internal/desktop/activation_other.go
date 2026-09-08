//go:build !darwin || !cgo

package desktop

var platformFocusClient func(int) error
var platformQuitClient func(int) error

func installActivationHandler(func(), func()) {}
func removeActivationHandler()                {}
