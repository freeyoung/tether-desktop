# Protocol notes

`go/core` follows these rules, so that several computers can sync without loops.

- The name in the certificate tells the kind of peer: `Tether-iOS` is the iPhone app, `Tether` is a computer. Send `clipboard_updated` to phones and `clipboard_set` to computers; `tetherd` ignores `clipboard_updated`.
- Forward content to every other peer, never back to the sender. Drop content with the same digest as the last content (trailing whitespace is ignored).
- Keep one session for each peer. When two computers dial each other at the same time, both keep the session that the computer with the smaller fingerprint (compared as hex text) dialed, so they always keep the same one.
- Redial a computer after a drop. Try all of its addresses, the last one that worked first. Skip an address that another connected computer holds (a dual-boot laptop).
- Close all sessions on quit: `tetherd` before 0.2.36 has no keepalive.
- The iPhone app sends no `file_status` and keeps a received file in memory, so send it at most 512 MiB.
