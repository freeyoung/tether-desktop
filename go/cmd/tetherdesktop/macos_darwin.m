#import <Cocoa/Cocoa.h>
#import <ServiceManagement/ServiceManagement.h>
#import <SystemConfiguration/SystemConfiguration.h>
#import <UserNotifications/UserNotifications.h>
#include <arpa/inet.h>
#include <dns_sd.h>
#include <os/log.h>

#include "macos_darwin.h"
#include "_cgo_export.h"

// MARK: - The main thread

// Runs blocks on the main thread in the common run loop modes. A GCD block
// on the main queue would not run while a dialog runs inside another one.
@interface TDRunner : NSObject
- (void)run:(dispatch_block_t)block;
@end

@implementation TDRunner
- (void)run:(dispatch_block_t)block {
  block();
}
@end

static void onMain(BOOL wait, dispatch_block_t block) {
  if ([NSThread isMainThread]) {
    block();
    return;
  }
  static TDRunner *runner;
  static dispatch_once_t once;
  dispatch_once(&once, ^{
    runner = [TDRunner new];
  });
  [runner performSelectorOnMainThread:@selector(run:)
                           withObject:block
                        waitUntilDone:wait
                                modes:@[ NSRunLoopCommonModes ]];
}

static char *copyString(NSString *s) {
  return s == nil ? NULL : strdup(s.UTF8String);
}

static char *copyData(NSData *data, int64_t *length) {
  if (data == nil) {
    return NULL;
  }
  char *out = malloc(data.length > 0 ? data.length : 1);
  memcpy(out, data.bytes, data.length);
  *length = (int64_t)data.length;
  return out;
}

static NSString *str(const char *s) {
  return s == NULL ? @"" : [NSString stringWithUTF8String:s];
}

// MARK: - Logs

void td_log(const char *line) {
  static os_log_t log;
  static dispatch_once_t once;
  dispatch_once(&once, ^{
    log = os_log_create("TetherDesktop", "core");
  });
  os_log(log, "%{public}s", line);
}

char *td_computer_name(void) {
  NSString *name = CFBridgingRelease(SCDynamicStoreCopyComputerName(NULL, NULL));
  return copyString(name.length > 0 ? name : NSProcessInfo.processInfo.hostName);
}

// MARK: - Notifications, the Finder service and wake

@interface TDDelegate : NSObject <UNUserNotificationCenterDelegate>
@end

@implementation TDDelegate

// Clicking "File from ..." shows the file in Finder.
- (void)userNotificationCenter:(UNUserNotificationCenter *)center
    didReceiveNotificationResponse:(UNNotificationResponse *)response
             withCompletionHandler:(void (^)(void))completionHandler {
  NSString *path = response.notification.request.content.userInfo[@"reveal"];
  if (path.length > 0) {
    [NSWorkspace.sharedWorkspace activateFileViewerSelectingURLs:@[ [NSURL fileURLWithPath:path] ]];
  }
  completionHandler();
}

// Show banners while the menu is open too.
- (void)userNotificationCenter:(UNUserNotificationCenter *)center
       willPresentNotification:(UNNotification *)notification
         withCompletionHandler:(void (^)(UNNotificationPresentationOptions))completionHandler {
  completionHandler(UNNotificationPresentationOptionBanner | UNNotificationPresentationOptionList);
}

// "Send with Tether Desktop" in Finder (NSServices in Info.plist): the
// selected files arrive as file URLs.
- (void)sendFiles:(NSPasteboard *)pboard userData:(NSString *)userData error:(NSString **)error {
  NSArray<NSURL *> *urls = [pboard readObjectsForClasses:@[ NSURL.class ]
                                                 options:@{NSPasteboardURLReadingFileURLsOnlyKey : @YES}];
  NSMutableArray *paths = [NSMutableArray array];
  for (NSURL *url in urls) {
    [paths addObject:url.path];
  }
  if (paths.count > 0) {
    tdServiceFiles((char *)[paths componentsJoinedByString:@"\n"].UTF8String);
  }
}

@end

static TDDelegate *delegate;

void td_init(void) {
  onMain(YES, ^{
    delegate = [TDDelegate new];
    if (NSBundle.mainBundle.bundleIdentifier != nil) {
      UNUserNotificationCenter *center = UNUserNotificationCenter.currentNotificationCenter;
      center.delegate = delegate;
      [center requestAuthorizationWithOptions:UNAuthorizationOptionAlert
                            completionHandler:^(BOOL granted, NSError *error){
                            }];
    }
    NSApp.servicesProvider = delegate;
    NSUpdateDynamicServices();
    // Sessions with computers do not survive a long sleep.
    [NSWorkspace.sharedWorkspace.notificationCenter addObserverForName:NSWorkspaceDidWakeNotification
                                                                object:nil
                                                                 queue:NSOperationQueue.mainQueue
                                                            usingBlock:^(NSNotification *note) {
                                                              tdDidWake();
                                                            }];
  });
}

void td_notify(const char *title, const char *body, const char *reveal) {
  if (NSBundle.mainBundle.bundleIdentifier == nil) {
    return;
  }
  UNMutableNotificationContent *content = [UNMutableNotificationContent new];
  content.title = str(title);
  content.body = str(body);
  NSString *path = str(reveal);
  if (path.length > 0) {
    content.userInfo = @{@"reveal" : path};
  }
  UNNotificationRequest *request = [UNNotificationRequest requestWithIdentifier:NSUUID.UUID.UUIDString
                                                                        content:content
                                                                        trigger:nil];
  [UNUserNotificationCenter.currentNotificationCenter addNotificationRequest:request withCompletionHandler:nil];
}

// MARK: - The pasteboard

uint64_t td_change_count(void) {
  __block uint64_t count;
  onMain(YES, ^{
    count = (uint64_t)NSPasteboard.generalPasteboard.changeCount;
  });
  return count;
}

char *td_pasteboard_types(void) {
  __block char *out;
  onMain(YES, ^{
    NSArray<NSPasteboardType> *types = NSPasteboard.generalPasteboard.types ?: @[];
    out = copyString([types componentsJoinedByString:@"\n"]);
  });
  return out;
}

char *td_pasteboard_text(int64_t *length) {
  __block char *out;
  onMain(YES, ^{
    NSString *text = [NSPasteboard.generalPasteboard stringForType:NSPasteboardTypeString];
    out = text.length > 0 ? copyData([text dataUsingEncoding:NSUTF8StringEncoding], length) : NULL;
  });
  return out;
}

char *td_pasteboard_png(int64_t *length) {
  __block char *out;
  onMain(YES, ^{
    NSPasteboard *pb = NSPasteboard.generalPasteboard;
    NSData *png = [pb dataForType:NSPasteboardTypePNG];
    if (png == nil) {
      // TIFF, JPEG or HEIC: let NSImage read it and write PNG.
      NSImage *image = [[NSImage alloc] initWithPasteboard:pb];
      NSBitmapImageRep *rep = image ? [NSBitmapImageRep imageRepWithData:image.TIFFRepresentation] : nil;
      png = [rep representationUsingType:NSBitmapImageFileTypePNG properties:@{}];
    }
    out = copyData(png, length);
  });
  return out;
}

uint64_t td_write_text(const char *data, int64_t length) {
  NSString *text = [[NSString alloc] initWithBytes:data length:(NSUInteger)length encoding:NSUTF8StringEncoding];
  __block uint64_t count;
  onMain(YES, ^{
    NSPasteboard *pb = NSPasteboard.generalPasteboard;
    [pb clearContents];
    [pb setString:text ?: @"" forType:NSPasteboardTypeString];
    count = (uint64_t)pb.changeCount;
  });
  return count;
}

uint64_t td_write_png(const char *data, int64_t length) {
  NSData *png = [NSData dataWithBytes:data length:(NSUInteger)length];
  __block uint64_t count;
  onMain(YES, ^{
    NSPasteboard *pb = NSPasteboard.generalPasteboard;
    [pb clearContents];
    // PNG for apps that read it, TIFF for the many that only read TIFF.
    [pb setData:png forType:NSPasteboardTypePNG];
    NSData *tiff = [[NSImage alloc] initWithData:png].TIFFRepresentation;
    if (tiff != nil) {
      [pb setData:tiff forType:NSPasteboardTypeTIFF];
    }
    count = (uint64_t)pb.changeCount;
  });
  return count;
}

// MARK: - Dialogs

// The first paragraph is the heading, the rest the text.
static NSAlert *makeAlert(NSString *text) {
  NSAlert *alert = [NSAlert new];
  NSRange split = [text rangeOfString:@"\n\n"];
  if (split.location == NSNotFound) {
    alert.messageText = text;
  } else {
    alert.messageText = [text substringToIndex:split.location];
    alert.informativeText = [text substringFromIndex:NSMaxRange(split)];
  }
  return alert;
}

void td_show_error(const char *text) {
  NSString *message = str(text);
  onMain(YES, ^{
    NSAlert *alert = [NSAlert new];
    alert.alertStyle = NSAlertStyleWarning;
    alert.messageText = @"Tether Desktop";
    alert.informativeText = message;
    [NSApp activate];
    [alert runModal];
  });
}

void td_show_about(const char *url) {
  NSString *link = str(url);
  onMain(NO, ^{
    NSMutableAttributedString *credits = [[NSMutableAttributedString alloc]
        initWithString:@"A desktop peer for Tether: the clipboard and files, shared between your phones and your computers.\n\n"
            attributes:@{NSFontAttributeName : [NSFont systemFontOfSize:11]}];
    [credits appendAttributedString:[[NSAttributedString alloc]
                                        initWithString:link
                                            attributes:@{NSLinkAttributeName : [NSURL URLWithString:link],
                                                         NSFontAttributeName : [NSFont systemFontOfSize:11]}]];
    NSMutableParagraphStyle *center = [NSMutableParagraphStyle new];
    center.alignment = NSTextAlignmentCenter;
    [credits addAttribute:NSParagraphStyleAttributeName value:center range:NSMakeRange(0, credits.length)];
    [NSApp activate];
    [NSApp orderFrontStandardAboutPanelWithOptions:@{NSAboutPanelOptionCredits : credits}];
  });
}

static int64_t currentPairing;

int td_ask_pairing(const char *text, int64_t token) {
  NSString *message = str(text);
  __block int choice = 0;
  onMain(YES, ^{
    NSAlert *alert = makeAlert(message);
    [alert addButtonWithTitle:@"Allow"];
    [alert addButtonWithTitle:@"Deny"];
    [alert addButtonWithTitle:@"Later"];
    currentPairing = token;
    [NSApp activate];
    NSModalResponse response = [alert runModal];
    currentPairing = 0;
    if (response == NSAlertFirstButtonReturn) {
      choice = 1;
    } else if (response == NSAlertSecondButtonReturn) {
      choice = 2;
    }
  });
  return choice;
}

// The request was answered elsewhere, or lapsed: close its prompt.
void td_cancel_pairing(int64_t token) {
  onMain(NO, ^{
    if (currentPairing == token) {
      [NSApp abortModal];
    }
  });
}

char *td_select_files(void) {
  __block char *out = NULL;
  onMain(YES, ^{
    NSOpenPanel *panel = [NSOpenPanel openPanel];
    panel.allowsMultipleSelection = YES;
    panel.canChooseDirectories = NO;
    panel.prompt = @"Send";
    [NSApp activate];
    if ([panel runModal] == NSModalResponseOK && panel.URLs.count > 0) {
      NSMutableArray *paths = [NSMutableArray array];
      for (NSURL *url in panel.URLs) {
        [paths addObject:url.path];
      }
      out = copyString([paths componentsJoinedByString:@"\n"]);
    }
  });
  return out;
}

int td_choose_one(const char *text, const char *names, const char *ok) {
  NSString *message = str(text), *okTitle = str(ok);
  NSArray<NSString *> *items = [str(names) componentsSeparatedByString:@"\n"];
  __block int choice = -1;
  onMain(YES, ^{
    NSAlert *alert = makeAlert(message);
    NSPopUpButton *popup = [[NSPopUpButton alloc] initWithFrame:NSMakeRect(0, 0, 260, 26) pullsDown:NO];
    [popup addItemsWithTitles:items];
    alert.accessoryView = popup;
    [alert addButtonWithTitle:okTitle];
    [alert addButtonWithTitle:@"Cancel"];
    [NSApp activate];
    if ([alert runModal] == NSAlertFirstButtonReturn) {
      choice = (int)popup.indexOfSelectedItem;
    }
  });
  return choice;
}

char *td_enter_text(const char *title, const char *text, const char *ok) {
  NSString *heading = str(title), *message = str(text), *okTitle = str(ok);
  __block char *out = NULL;
  onMain(YES, ^{
    NSAlert *alert = [NSAlert new];
    alert.messageText = heading;
    alert.informativeText = message;
    NSTextField *field = [[NSTextField alloc] initWithFrame:NSMakeRect(0, 0, 260, 24)];
    alert.accessoryView = field;
    [alert addButtonWithTitle:okTitle];
    [alert addButtonWithTitle:@"Cancel"];
    alert.window.initialFirstResponder = field;
    [NSApp activate];
    if ([alert runModal] == NSAlertFirstButtonReturn) {
      out = copyString(field.stringValue);
    }
  });
  return out;
}

// MARK: - The system

void td_open_url(const char *url) {
  NSURL *u = [NSURL URLWithString:str(url)];
  if (u != nil) {
    [NSWorkspace.sharedWorkspace openURL:u];
  }
}

// Marks a received file the way a browser marks a download, so Gatekeeper
// checks it before it runs.
void td_quarantine(const char *path) {
  NSURL *url = [NSURL fileURLWithPath:str(path)];
  NSDictionary *properties = @{
    (__bridge NSString *)kLSQuarantineAgentNameKey : @"Tether Desktop",
    (__bridge NSString *)kLSQuarantineTypeKey : (__bridge NSString *)kLSQuarantineTypeOtherDownload,
  };
  NSError *error = nil;
  if (![url setResourceValue:properties forKey:NSURLQuarantinePropertiesKey error:&error]) {
    td_log([NSString stringWithFormat:@"could not quarantine a received file: %@", error].UTF8String);
  }
}

int td_login_enabled(void) {
  return SMAppService.mainAppService.status == SMAppServiceStatusEnabled;
}

char *td_set_login(int on) {
  SMAppService *service = SMAppService.mainAppService;
  NSError *error = nil;
  BOOL ok = on ? [service registerAndReturnError:&error] : [service unregisterAndReturnError:&error];
  if (service.status == SMAppServiceStatusRequiresApproval) {
    [SMAppService openSystemSettingsLoginItems];
  }
  if (!ok) {
    return copyString([NSString stringWithFormat:@"macOS refused the change: %@", error.localizedDescription]);
  }
  return NULL;
}

// MARK: - DNS-SD

// 1 connection to mDNSResponder carries the registration, the browse and
// every resolve, so 1 socket serves them all. Only the thread that runs
// td_dnssd_process uses it.
static DNSServiceRef connection;

typedef struct {
  char *name;
  char *fingerprint;
  int port;
  int computer;
} found;

static void freeFound(found *f) {
  free(f->name);
  free(f->fingerprint);
  free(f);
}

static void registered(DNSServiceRef ref, DNSServiceFlags flags, DNSServiceErrorType error, const char *name,
                       const char *type, const char *domain, void *context) {
  if (error != kDNSServiceErr_NoError) {
    td_log([NSString stringWithFormat:@"dns-sd: could not advertise: error %d", error].UTF8String);
  }
}

static void addressed(DNSServiceRef ref, DNSServiceFlags flags, uint32_t interface, DNSServiceErrorType error,
                      const char *host, const struct sockaddr *address, uint32_t ttl, void *context) {
  found *f = context;
  if (error == kDNSServiceErr_NoError && (flags & kDNSServiceFlagsAdd) && address != NULL &&
      address->sa_family == AF_INET) {
    char ip[INET_ADDRSTRLEN];
    inet_ntop(AF_INET, &((const struct sockaddr_in *)address)->sin_addr, ip, sizeof ip);
    tdResolved(f->name, f->fingerprint, ip, f->port, f->computer);
  }
  if (!(flags & kDNSServiceFlagsMoreComing)) {
    DNSServiceRefDeallocate(ref);
    freeFound(f);
  }
}

static void resolved(DNSServiceRef ref, DNSServiceFlags flags, uint32_t interface, DNSServiceErrorType error,
                     const char *fullname, const char *host, uint16_t port, uint16_t txtLength,
                     const unsigned char *txt, void *context) {
  char *name = context;
  uint8_t fpLength = 0;
  const void *fp = TXTRecordGetValuePtr(txtLength, txt, "fp", &fpLength);
  if (error == kDNSServiceErr_NoError && fp != NULL && fpLength > 0) {
    found *f = calloc(1, sizeof(found));
    f->name = strdup(name);
    f->fingerprint = strndup(fp, fpLength);
    f->port = ntohs(port);
    // tetherd and Tether Desktop publish v=1; the phone does not.
    f->computer = TXTRecordContainsKey(txtLength, txt, "v");
    DNSServiceRef lookup = connection;
    if (DNSServiceGetAddrInfo(&lookup, kDNSServiceFlagsShareConnection, interface, kDNSServiceProtocol_IPv4, host,
                              addressed, f) != kDNSServiceErr_NoError) {
      freeFound(f);
    }
  }
  DNSServiceRefDeallocate(ref);
  free(name);
}

static void browsed(DNSServiceRef ref, DNSServiceFlags flags, uint32_t interface, DNSServiceErrorType error,
                    const char *name, const char *type, const char *domain, void *context) {
  if (error != kDNSServiceErr_NoError) {
    return;
  }
  if (!(flags & kDNSServiceFlagsAdd)) {
    tdBrowseRemoved((char *)name, interface);
    return;
  }
  tdBrowseAdded((char *)name, interface);
  char *copy = strdup(name);
  DNSServiceRef resolve = connection;
  if (DNSServiceResolve(&resolve, kDNSServiceFlagsShareConnection, interface, name, type, domain, resolved, copy) !=
      kDNSServiceErr_NoError) {
    free(copy);
  }
}

int td_dnssd_start(const char *name, int port, const char *fingerprint) {
  if (DNSServiceCreateConnection(&connection) != kDNSServiceErr_NoError) {
    connection = NULL;
    return -1;
  }
  TXTRecordRef txt;
  TXTRecordCreate(&txt, 0, NULL);
  TXTRecordSetValue(&txt, "fp", (uint8_t)strlen(fingerprint), fingerprint);
  TXTRecordSetValue(&txt, "v", 1, "1");
  DNSServiceRef registration = connection;
  DNSServiceErrorType error =
      DNSServiceRegister(&registration, kDNSServiceFlagsShareConnection, 0, name, "_tether._tcp", NULL, NULL,
                         htons((uint16_t)port), TXTRecordGetLength(&txt), TXTRecordGetBytesPtr(&txt), registered, NULL);
  TXTRecordDeallocate(&txt);
  DNSServiceRef browse = connection;
  if (error == kDNSServiceErr_NoError) {
    error = DNSServiceBrowse(&browse, kDNSServiceFlagsShareConnection, 0, "_tether._tcp", NULL, browsed, NULL);
  }
  if (error != kDNSServiceErr_NoError) {
    td_log([NSString stringWithFormat:@"dns-sd: error %d", error].UTF8String);
    DNSServiceRefDeallocate(connection);
    connection = NULL;
    return -1;
  }
  return DNSServiceRefSockFD(connection);
}

int td_dnssd_process(void) {
  return connection == NULL ? -1 : DNSServiceProcessResult(connection);
}

// Ends the registration, the browse and every resolve with the connection.
void td_dnssd_stop(void) {
  if (connection != NULL) {
    DNSServiceRefDeallocate(connection);
    connection = NULL;
  }
}
