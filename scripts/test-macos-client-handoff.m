// CI-only two-process fixture. Links both applications' production native code.
#import <Cocoa/Cocoa.h>
#import <fcntl.h>
#import <unistd.h>

extern int cineko_activate_client(int pid);
extern int cineko_configure_activation_policy(int foreground);
extern void cineko_install_activation_observer(void);
extern void cineko_remove_activation_observer(void);
extern int cineko_request_client_quit(int pid);

static int clientPID;
static int activationFailures;
void cinekoLauncherQuitRequested(void) { cineko_request_client_quit(clientPID); }
void cinekoLauncherActivated(void) {
    if (clientPID > 0 && !cineko_activate_client(clientPID)) activationFailures++;
}

static void pump(void) {
    NSEvent *event = [NSApp nextEventMatchingMask:NSEventMaskAny
        untilDate:[NSDate dateWithTimeIntervalSinceNow:0.05] inMode:NSDefaultRunLoopMode dequeue:YES];
    if (event) [NSApp sendEvent:event];
}

static BOOL waitFor(int fd, NSString *expected) {
    NSMutableString *received = [NSMutableString string];
    NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:5];
    while ([deadline timeIntervalSinceNow] > 0) {
        char bytes[1024];
        ssize_t count = read(fd, bytes, sizeof(bytes));
        if (count > 0) {
            NSString *part = [[NSString alloc] initWithBytes:bytes length:count encoding:NSUTF8StringEncoding];
            if (part) [received appendString:part];
            [part release];
            if ([received containsString:expected]) return YES;
        }
        pump();
    }
    fprintf(stderr, "Expected %s, received %s\n", expected.UTF8String, received.UTF8String);
    return NO;
}

static void runClient(void) {
    NSWindow *window = [[NSWindow alloc] initWithContentRect:NSMakeRect(150, 150, 500, 300)
        styleMask:NSWindowStyleMaskTitled | NSWindowStyleMaskMiniaturizable
        backing:NSBackingStoreBuffered defer:NO];
    window.title = @"Cineko Client fixture";
    [window makeKeyAndOrderFront:nil];
    cineko_configure_activation_policy(0);
    dispatch_async(dispatch_get_global_queue(QOS_CLASS_USER_INITIATED, 0), ^{
        char line[64];
        while (fgets(line, sizeof(line), stdin)) {
            char command = line[0];
            dispatch_async(dispatch_get_main_queue(), ^{
                if (command == 'm') [window miniaturize:nil];
                if (command == 'h') [NSApp hide:nil];
                dispatch_after(dispatch_time(DISPATCH_TIME_NOW, 500 * NSEC_PER_MSEC), dispatch_get_main_queue(), ^{
                    printf("%c mini=%d visible=%d active=%d key=%d\n", command,
                        window.miniaturized, window.visible, NSApp.active, window.keyWindow);
                    fflush(stdout);
                });
            });
        }
    });
    puts("ready");
    fflush(stdout);
    [NSApp run];
}

int main(int argc, const char **argv) {
    @autoreleasepool {
        [NSApplication sharedApplication];
        [NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
        [NSApp finishLaunching];
        if (argc > 1) { runClient(); return 0; }
        NSTask *client = [[NSTask alloc] init];
        client.executableURL = [NSURL fileURLWithPath:[NSString stringWithUTF8String:argv[0]]];
        client.arguments = @[@"--client"];
        NSPipe *input = [NSPipe pipe], *output = [NSPipe pipe];
        client.standardInput = input;
        client.standardOutput = output;
        NSError *error = nil;
        if (![client launchAndReturnError:&error]) { NSLog(@"%@", error); return 1; }
        int fd = output.fileHandleForReading.fileDescriptor;
        fcntl(fd, F_SETFL, fcntl(fd, F_GETFL) | O_NONBLOCK);
        BOOL passed = waitFor(fd, @"ready");
        clientPID = client.processIdentifier;
        cineko_install_activation_observer();
        for (NSString *mode in @[@"q", @"m", @"h", @"m"]) {
            [input.fileHandleForWriting writeData:[[mode stringByAppendingString:@"\n"] dataUsingEncoding:NSUTF8StringEncoding]];
            passed = waitFor(fd, [mode stringByAppendingString:@" mini="]) && passed;
            // The first event is native app activation (Dock/app switcher);
            // the explicit callback also exercises repeated reopen requests.
            [NSApp activateIgnoringOtherApps:YES];
            for (int i = 0; i < 4; i++) pump();
            cinekoLauncherActivated();
            [input.fileHandleForWriting writeData:[@"q\n" dataUsingEncoding:NSUTF8StringEncoding]];
            passed = waitFor(fd, @"q mini=0 visible=1 active=1 key=1") && passed;
        }
        cineko_remove_activation_observer();
        cinekoLauncherQuitRequested();
        NSDate *exitDeadline = [NSDate dateWithTimeIntervalSinceNow:5];
        while (client.running && exitDeadline.timeIntervalSinceNow > 0) pump();
        if (client.running) {
            fprintf(stderr, "Client did not honor the status-menu quit request\n");
            [client terminate];
            passed = NO;
        }
        [client waitUntilExit];
        if (!passed || activationFailures) return 1;
        puts("PASS: separate-process handoff restores frontmost key window (normal, minimized, hidden, repeated)");
    }
    return 0;
}
