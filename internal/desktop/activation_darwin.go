//go:build darwin && cgo

package desktop

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
void cineko_install_activation_observer(void);
void cineko_remove_activation_observer(void);
int cineko_activate_client(int pid);
int cineko_request_client_quit(int pid);
*/
import "C"

import (
	"fmt"
	"sync"
)

var activationHandler struct {
	sync.RWMutex
	callback func()
	quit     func()
}

func installActivationHandler(callback, quit func()) {
	activationHandler.Lock()
	activationHandler.callback = callback
	activationHandler.quit = quit
	activationHandler.Unlock()
	C.cineko_install_activation_observer()
}

func removeActivationHandler() {
	activationHandler.Lock()
	activationHandler.callback = nil
	activationHandler.quit = nil
	activationHandler.Unlock()
	C.cineko_remove_activation_observer()
}

//export cinekoLauncherQuitRequested
func cinekoLauncherQuitRequested() {
	activationHandler.RLock()
	quit := activationHandler.quit
	activationHandler.RUnlock()
	if quit != nil {
		go quit()
	}
}

var platformQuitClient = func(pid int) error {
	if C.cineko_request_client_quit(C.int(pid)) == 0 {
		return fmt.Errorf("Client pid %d is not running", pid)
	}
	return nil
}

//export cinekoLauncherActivated
func cinekoLauncherActivated() {
	activationHandler.RLock()
	callback := activationHandler.callback
	activationHandler.RUnlock()
	if callback != nil {
		// Native events arrive on the AppKit thread; do not synchronously call
		// back into AppKit from that thread through Go.
		go callback()
	}
}

var platformFocusClient = func(pid int) error {
	if C.cineko_activate_client(C.int(pid)) == 0 {
		return fmt.Errorf("macOS refused activation of Client pid %d", pid)
	}
	return nil
}
