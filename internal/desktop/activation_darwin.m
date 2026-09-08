//go:build darwin && cgo

#import <Cocoa/Cocoa.h>
#import <pthread.h>

extern void cinekoLauncherActivated(void);

@interface CinekoActivationObserver : NSObject
- (void)activated:(NSNotification *)notification;
- (void)reopen:(NSAppleEventDescriptor *)event reply:(NSAppleEventDescriptor *)reply;
@end

@implementation CinekoActivationObserver
- (void)activated:(NSNotification *)notification { cinekoLauncherActivated(); }
- (void)reopen:(NSAppleEventDescriptor *)event reply:(NSAppleEventDescriptor *)reply {
    cinekoLauncherActivated();
}
@end

static CinekoActivationObserver *activationObserver;

static void onMainThread(dispatch_block_t action) {
    if (pthread_main_np()) action();
    else dispatch_sync(dispatch_get_main_queue(), action);
}

void cineko_install_activation_observer(void) {
    onMainThread(^{
        if (activationObserver != nil) return;
        activationObserver = [[CinekoActivationObserver alloc] init];
        [[NSNotificationCenter defaultCenter] addObserver:activationObserver
            selector:@selector(activated:) name:NSApplicationDidBecomeActiveNotification object:NSApp];
        [[NSAppleEventManager sharedAppleEventManager] setEventHandler:activationObserver
            andSelector:@selector(reopen:reply:) forEventClass:kCoreEventClass andEventID:kAEReopenApplication];
    });
}

void cineko_remove_activation_observer(void) {
    onMainThread(^{
        if (activationObserver == nil) return;
        [[NSNotificationCenter defaultCenter] removeObserver:activationObserver];
        [[NSAppleEventManager sharedAppleEventManager] removeEventHandlerForEventClass:kCoreEventClass
            andEventID:kAEReopenApplication];
        [activationObserver release];
        activationObserver = nil;
    });
}

int cineko_activate_client(int pid) {
    if (pid <= 0) return 0;
    __block BOOL activated = NO;
    onMainThread(^{
        NSRunningApplication *client = [NSRunningApplication runningApplicationWithProcessIdentifier:pid];
        if (client == nil || client.terminated) return;
        if (@available(macOS 14.0, *)) {
            [NSApp yieldActivationToApplication:client];
        }
        [client unhide];
        activated = [client activateWithOptions:NSApplicationActivateAllWindows | NSApplicationActivateIgnoringOtherApps];
    });
    return activated ? 1 : 0;
}
