package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"path/filepath"

	"fyne.io/systray"
	"github.com/ncruces/zenity"

	"github.com/freeyoung/tether-core-go/core"
)

// The Windows side of tray.go: zenity dialogs, toasts, and the menu items
// for Explorer's Send To and the firewall.

const thisDevice = "PC"

func addPlatformItems(a *app) {
	onClick(systray.AddMenuItemCheckbox("Show in Explorer's Send To Menu", "", sendToEnabled()), func() {
		a.toggle(func() error { return setSendTo(!sendToEnabled()) })
	})
	onClick(systray.AddMenuItem("Firewall Help…", ""), a.firewallHelp)
}

func showError(text string) { _ = zenity.Error(text, zenity.Title(title)) }

func askPairing(ctx context.Context, text string) answer {
	err := zenity.Question(text, zenity.Title(title), zenity.OKLabel("Allow"), zenity.ExtraButton("Deny"),
		zenity.CancelLabel("Later"), zenity.NoWrap(), zenity.Context(ctx))
	switch err {
	case nil:
		return allow
	case zenity.ErrExtraButton:
		return deny
	}
	return later
}

func selectFiles() []string {
	files, err := zenity.SelectFileMultiple(zenity.Title("Send Files"))
	if err != nil {
		return nil
	}
	return files
}

func chooseOne(text string, names []string) (int, bool) {
	choice, err := zenity.List(text, names, zenity.Title(title), zenity.OKLabel("Send"))
	if err != nil {
		return 0, false
	}
	for i, name := range names {
		if name == choice {
			return i, true
		}
	}
	return 0, false
}

func enterText(heading, text, ok string) (string, bool) {
	value, err := zenity.Entry(text, zenity.Title(heading), zenity.OKLabel(ok))
	return value, err == nil
}

// notifyFile: clicking the toast opens the folder the file is in.
func notifyFile(heading, text, path string) { notifyOpening(heading, text, filepath.Dir(path)) }

func (a *app) firewallHelp() {
	command := fmt.Sprintf(`netsh advfirewall firewall add rule name="Tether Desktop" dir=in action=allow program="%s" protocol=TCP localport=%d profile=private`,
		executable(), core.DefaultPort)
	a.clipboard.Write(core.TextContent(command))
	_ = zenity.Info("The iPhone and other computers connect to this PC on TCP port 5134.\n\n"+
		"If Windows asked whether to allow Tether Desktop, allow it on private networks. "+
		"Make sure this network is set as Private in Settings > Network & internet.\n\n"+
		"To allow it by hand, run this in an administrator terminal (it is on the clipboard now):\n\n"+command,
		zenity.Title("Firewall Help"))
}

// menuDot is a small filled circle for the device list, as an .ico (Windows
// menus take icons in that form). It has 1 image per display scale, from
// 100% to 300%, so the menu never stretches one.
func menuDot(c color.NRGBA) []byte {
	sizes := []int{16, 20, 24, 28, 32, 40, 48}
	var images [][]byte
	for _, size := range sizes {
		images = append(images, dotPNG(size, c))
	}
	var ico bytes.Buffer
	_ = binary.Write(&ico, binary.LittleEndian, []uint16{0, 1, uint16(len(sizes))})
	offset := 6 + 16*len(sizes)
	for i, size := range sizes {
		ico.Write([]byte{byte(size), byte(size), 0, 0})
		_ = binary.Write(&ico, binary.LittleEndian, []uint16{1, 32})
		_ = binary.Write(&ico, binary.LittleEndian, []uint32{uint32(len(images[i])), uint32(offset)})
		offset += len(images[i])
	}
	for _, img := range images {
		ico.Write(img)
	}
	return ico.Bytes()
}

// dotPNG draws a circle a quarter of the size across, with smooth edges:
// each pixel is covered by 4 x 4 samples.
func dotPNG(size int, c color.NRGBA) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	center, radius := float64(size)/2, float64(size)/4
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			inside := 0
			for sy := 0; sy < 4; sy++ {
				for sx := 0; sx < 4; sx++ {
					dx := float64(x) + (float64(sx)+0.5)/4 - center
					dy := float64(y) + (float64(sy)+0.5)/4 - center
					if dx*dx+dy*dy <= radius*radius {
						inside++
					}
				}
			}
			if inside > 0 {
				img.SetNRGBA(x, y, color.NRGBA{c.R, c.G, c.B, uint8(int(c.A) * inside / 16)})
			}
		}
	}
	var out bytes.Buffer
	_ = png.Encode(&out, img)
	return out.Bytes()
}
