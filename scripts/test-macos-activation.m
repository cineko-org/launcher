#import <Cocoa/Cocoa.h>
#import "../internal/desktop/activation_darwin.m"

static int activationCount;
static int quitCount;
void cinekoLauncherActivated(void) { activationCount++; }
void cinekoLauncherQuitRequested(void) { quitCount++; }

int main(void) {
    @autoreleasepool {
        [NSApplication sharedApplication];
        [NSApp finishLaunching];
        cineko_install_activation_observer();
        cineko_install_activation_observer();
        [[NSNotificationCenter defaultCenter] postNotificationName:NSApplicationDidBecomeActiveNotification object:NSApp];
        NSCAssert(activationCount == 1, @"duplicate activation observer");
        [activationObserver reopen:nil reply:nil];
        NSCAssert(activationCount == 2, @"reopen was not forwarded");
        NSCAssert(statusItem != nil && statusItem.menu.numberOfItems == 3, @"missing status menu");
        [statusItem.menu performActionForItemAtIndex:0];
        NSCAssert(activationCount == 3, @"status menu did not open Client");
        [statusItem.menu performActionForItemAtIndex:2];
        NSCAssert(quitCount == 1, @"status menu did not request quit");
        NSCAssert(cineko_activate_client(0) == 0, @"accepted invalid PID");
        NSCAssert(cineko_activate_client(-1) == 0, @"accepted negative PID");
        NSCAssert(cineko_activate_client(999999) == 0, @"accepted absent Client");
        cineko_remove_activation_observer();
        cineko_remove_activation_observer();
        [[NSNotificationCenter defaultCenter] postNotificationName:NSApplicationDidBecomeActiveNotification object:NSApp];
        NSCAssert(activationCount == 3 && statusItem == nil, @"activation observer or status item survived shutdown");
        puts("PASS: Dock/app-switch activation, reopen, singleton observer, invalid/dead PID, shutdown");
    }
    return 0;
}
