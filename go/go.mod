module tetherdesktop

go 1.27.1

require (
	fyne.io/systray v1.12.2
	github.com/Microsoft/go-winio v0.6.2
	github.com/freeyoung/tether-core-go v0.3.0
	github.com/go-ole/go-ole v1.3.0
	github.com/ncruces/zenity v0.10.15
	golang.org/x/sys v0.49.0
)

require (
	github.com/akavel/rsrc v0.10.2 // indirect
	github.com/dchest/jsmin v1.0.0 // indirect
	github.com/godbus/dbus/v5 v5.1.0 // indirect
	github.com/josephspurrier/goversioninfo v1.7.0 // indirect
	github.com/miekg/dns v1.1.73 // indirect
	github.com/randall77/makefat v0.0.0-20260406194835-1b91746796b7 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/image v0.44.0 // indirect
	golang.org/x/net v0.61.0 // indirect
	software.sslmate.com/src/go-pkcs12 v0.7.4 // indirect
)

// A patched copy: see third_party/systray/PATCHED.md.
replace fyne.io/systray => ./third_party/systray
