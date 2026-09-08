//go:build darwin && cgo

#import <Cocoa/Cocoa.h>
#import <pthread.h>

extern void cinekoLauncherActivated(void);
extern void cinekoLauncherQuitRequested(void);

@interface CinekoActivationObserver : NSObject
- (void)activated:(NSNotification *)notification;
- (void)reopen:(NSAppleEventDescriptor *)event reply:(NSAppleEventDescriptor *)reply;
- (void)showClient:(id)sender;
- (void)quitClient:(id)sender;
@end

@implementation CinekoActivationObserver
- (void)activated:(NSNotification *)notification { cinekoLauncherActivated(); }
- (void)showClient:(id)sender { cinekoLauncherActivated(); }
- (void)quitClient:(id)sender { cinekoLauncherQuitRequested(); }
- (void)reopen:(NSAppleEventDescriptor *)event reply:(NSAppleEventDescriptor *)reply {
    cinekoLauncherActivated();
}
@end

static CinekoActivationObserver *activationObserver;
static NSStatusItem *statusItem;

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
        statusItem = [[[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength] retain];
        statusItem.button.toolTip = @"Cineko";
        if (@available(macOS 11.0, *)) {
            NSImage *icon = [NSImage imageWithSystemSymbolName:@"ticket" accessibilityDescription:@"Cineko"];
            icon.template = YES;
            statusItem.button.image = icon;
        }
        if (statusItem.button.image == nil) statusItem.button.title = @"Cineko";
        NSMenu *menu = [[NSMenu alloc] initWithTitle:@"Cineko"];
        NSMenuItem *show = [menu addItemWithTitle:@"Cineko 열기" action:@selector(showClient:) keyEquivalent:@""];
        show.target = activationObserver;
        [menu addItem:[NSMenuItem separatorItem]];
        NSMenuItem *quit = [menu addItemWithTitle:@"종료" action:@selector(quitClient:) keyEquivalent:@""];
        quit.target = activationObserver;
        statusItem.menu = menu;
        [menu release];
    });
}

void cineko_remove_activation_observer(void) {
    onMainThread(^{
        if (activationObserver == nil) return;
        [[NSStatusBar systemStatusBar] removeStatusItem:statusItem];
        [statusItem release];
        statusItem = nil;
        [[NSNotificationCenter defaultCenter] removeObserver:activationObserver];
        [[NSAppleEventManager sharedAppleEventManager] removeEventHandlerForEventClass:kCoreEventClass
            andEventID:kAEReopenApplication];
        [activationObserver release];
        activationObserver = nil;
    });
}

int cineko_request_client_quit(int pid) {
    if (pid <= 0) return 0;
    NSRunningApplication *client = [NSRunningApplication runningApplicationWithProcessIdentifier:pid];
    if (client == nil || client.terminated) return 0;
    NSString *name = [NSString stringWithFormat:@"io.cineko.client.quit.%d", pid];
    [[NSDistributedNotificationCenter defaultCenter] postNotificationName:name object:nil userInfo:nil deliverImmediately:YES];
    return 1;
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
