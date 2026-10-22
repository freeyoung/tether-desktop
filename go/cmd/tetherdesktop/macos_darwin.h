// The macOS side of the tray app, in Objective-C (macos_darwin.m). Go calls
// these from its own threads. Each one that touches AppKit runs its work on
// the main thread, in the common run loop modes, so it also runs while a
// dialog is open. Strings that a function returns are malloc'ed; the caller
// frees them.
#include <stdint.h>

void td_init(void);
void td_log(const char *line);
char *td_computer_name(void);

// The pasteboard.
uint64_t td_change_count(void);
char *td_pasteboard_types(void);                // 1 type per line
char *td_pasteboard_text(int64_t *length);      // NULL for none
char *td_pasteboard_png(int64_t *length);       // NULL for none; other images become PNG
uint64_t td_write_text(const char *data, int64_t length);
uint64_t td_write_png(const char *data, int64_t length);

// Dialogs. They block the calling thread until the user answers.
void td_show_error(const char *text);
void td_show_about(const char *url);           // returns at once: the panel is not modal
int td_ask_pairing(const char *text, int64_t token); // 0 later, 1 allow, 2 deny
void td_cancel_pairing(int64_t token);
char *td_select_files(void);                    // 1 path per line, or NULL
int td_choose_one(const char *text, const char *names, const char *ok); // names: 1 per line; -1 for cancel
char *td_enter_text(const char *title, const char *text, const char *ok); // NULL for cancel

// The system.
void td_notify(const char *title, const char *body, const char *reveal); // reveal: a path, or ""
// A notification of an Android phone: a later one with the same id replaces it.
// peer and app go to tdPhoneNotificationClicked; code, if not "", gets a Copy Code button.
void td_phone_notify(const char *id, const char *thread, const char *title, const char *subtitle, const char *body,
                     const char *peer, const char *app, const char *code);
void td_phone_unnotify(const char *id);
void td_open_url(const char *url);

// The Messages window: a web view with the page, made on the first call and
// kept when closed. The page posts to tdWebMessage; td_messages_eval runs
// JavaScript in it.
void td_messages_show(const char *html, const char *title);
void td_messages_eval(const char *js);
void td_quarantine(const char *path);
int td_login_enabled(void);
char *td_set_login(int on);                     // an error message, or NULL

// DNS-SD through mDNSResponder: advertise _tether._tcp and browse for it.
// td_dnssd_start returns the socket to wait on, or -1.
int td_dnssd_start(const char *name, int port, const char *fingerprint);
int td_dnssd_process(void);
void td_dnssd_stop(void);
