// Draws the app icon and writes it for both apps:
//   assets/AppIcon.icns                        macOS
//   go/cmd/tetherdesktop/AppIcon.ico           Windows tray
//   go/cmd/tetherdesktop/winres/icon.png       Windows exe
//   assets/AppIcon.png                         1024 px, for the README
//   go/cmd/tetherdesktop/MenuBarIcon.png       macOS menu bar, a template, 2x
//
//   swift scripts/make-icons.swift
//
// The icon: a monitor with a phone in front of it, white on a blue rounded
// square. The menu bar shows the same devices alone, black on clear, and
// macOS tints them.
import AppKit

let root = URL(fileURLWithPath: CommandLine.arguments[0]).deletingLastPathComponent().deletingLastPathComponent()

// The monitor, behind, and the phone, in front, in a 375 x 270 box with the
// origin bottom left. The stand is under the middle of the screen. A margin around the phone is left out of the monitor,
// so the 2 shapes stay apart on any background.
let devicesBox = CGRect(x: 0, y: 0, width: 375, height: 270)

func devices(in ctx: CGContext, color: CGColor, line: CGFloat) {
    ctx.saveGState()
    ctx.setStrokeColor(color)
    ctx.setLineWidth(line)
    ctx.setLineCap(.round)
    ctx.setLineJoin(.round)

    let phone = CGRect(x: 0, y: 0, width: 120, height: 215)
    let gap = line * 1.1

    // Monitor and stand, clipped to outside the phone and its margin.
    ctx.saveGState()
    ctx.addRect(devicesBox.insetBy(dx: -line, dy: -line))
    ctx.addPath(CGPath(roundedRect: phone.insetBy(dx: -gap, dy: -gap), cornerWidth: 30 + gap,
                       cornerHeight: 30 + gap, transform: nil))
    ctx.clip(using: .evenOdd)
    let screen = CGRect(x: 105, y: 90, width: 270, height: 180)
    ctx.addPath(CGPath(roundedRect: screen, cornerWidth: 26, cornerHeight: 26, transform: nil))
    ctx.move(to: CGPoint(x: 240, y: 90))
    ctx.addLine(to: CGPoint(x: 240, y: 44))
    ctx.move(to: CGPoint(x: 190, y: 30))
    ctx.addLine(to: CGPoint(x: 290, y: 30))
    ctx.strokePath()
    ctx.restoreGState()

    ctx.addPath(CGPath(roundedRect: phone, cornerWidth: 30, cornerHeight: 30, transform: nil))
    ctx.strokePath()
    ctx.restoreGState()
}

// Draws devices() so that the box, with the line width, fills `target`.
func devices(in ctx: CGContext, color: CGColor, line: CGFloat, fitting target: CGRect) {
    let bounds = devicesBox.insetBy(dx: -line / 2, dy: -line / 2)
    let fit = min(target.width / bounds.width, target.height / bounds.height)
    ctx.saveGState()
    ctx.translateBy(x: target.midX - bounds.width * fit / 2, y: target.midY - bounds.height * fit / 2)
    ctx.scaleBy(x: fit, y: fit)
    ctx.translateBy(x: -bounds.minX, y: -bounds.minY)
    devices(in: ctx, color: color, line: line)
    ctx.restoreGState()
}

// Draws on a 1024 x 1024 canvas, origin bottom left. `small` thickens the
// lines for sizes where 1 px matters; `tiny` (24 px and less) also drops the
// shadow.
func draw(in ctx: CGContext, small: Bool, tiny: Bool) {
    // The body, on Apple's icon grid: 824 px with a 100 px margin.
    let body = CGRect(x: 100, y: 100, width: 824, height: 824)
    let shape = CGPath(roundedRect: body, cornerWidth: 185, cornerHeight: 185, transform: nil)
    ctx.saveGState()
    if !tiny {
        ctx.setShadow(offset: CGSize(width: 0, height: -12), blur: 28, color: CGColor(gray: 0, alpha: 0.35))
    }
    ctx.addPath(shape)
    ctx.setFillColor(CGColor(srgbRed: 0.08, green: 0.40, blue: 0.85, alpha: 1))
    ctx.fillPath()
    ctx.restoreGState()

    ctx.saveGState()
    ctx.addPath(shape)
    ctx.clip()
    let gradient = CGGradient(colorsSpace: CGColorSpace(name: CGColorSpace.sRGB),
                              colors: [CGColor(srgbRed: 0.20, green: 0.68, blue: 0.98, alpha: 1),
                                       CGColor(srgbRed: 0.07, green: 0.36, blue: 0.84, alpha: 1)] as CFArray,
                              locations: [0, 1])!
    ctx.drawLinearGradient(gradient, start: CGPoint(x: 512, y: 924), end: CGPoint(x: 512, y: 100), options: [])
    ctx.restoreGState()

    // Line widths in the units of devicesBox, about 30 px of 1024 at full size.
    let line: CGFloat = tiny ? 50 : small ? 38 : 24
    let width: CGFloat = tiny ? 560 : small ? 570 : 560
    devices(in: ctx, color: .white, line: line,
            fitting: CGRect(x: 512 - width / 2, y: 512 - width / 2, width: width, height: width))
}

func png(size: Int) -> Data {
    let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: size, pixelsHigh: size, bitsPerSample: 8,
                               samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB,
                               bytesPerRow: 0, bitsPerPixel: 0)!
    let ctx = NSGraphicsContext(bitmapImageRep: rep)!.cgContext
    ctx.interpolationQuality = .high
    let scale = CGFloat(size) / 1024
    ctx.scaleBy(x: scale, y: scale)
    if size <= 32 {
        // Small sizes: less margin, so the shape fills the pixels.
        let grow: CGFloat = size <= 24 ? 1.22 : 1.18
        ctx.translateBy(x: 512, y: 512)
        ctx.scaleBy(x: grow, y: grow)
        ctx.translateBy(x: -512, y: -512)
    }
    draw(in: ctx, small: size <= 48, tiny: size <= 24)
    return rep.representation(using: .png, properties: [:])!
}

// The menu bar icon: devices() alone, 16 pt high, black on clear.
func menuBarPNG(scale: CGFloat) -> Data {
    let line: CGFloat = 32
    let bounds = devicesBox.insetBy(dx: -line / 2, dy: -line / 2)
    let height: CGFloat = 16
    let width = (height * bounds.width / bounds.height).rounded()
    let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: Int(width * scale),
                               pixelsHigh: Int(height * scale), bitsPerSample: 8, samplesPerPixel: 4,
                               hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB,
                               bytesPerRow: 0, bitsPerPixel: 0)!
    let ctx = NSGraphicsContext(bitmapImageRep: rep)!.cgContext
    ctx.interpolationQuality = .high
    devices(in: ctx, color: .black, line: line,
            fitting: CGRect(x: 0, y: 0, width: width * scale, height: height * scale))
    // Set after drawing: a size in points scales the context.
    rep.size = NSSize(width: width, height: height)
    return rep.representation(using: .png, properties: [:])!
}

let fm = FileManager.default
let assets = root.appendingPathComponent("assets")
try fm.createDirectory(at: assets, withIntermediateDirectories: true)
try png(size: 1024).write(to: assets.appendingPathComponent("AppIcon.png"))
try menuBarPNG(scale: 2).write(to: root.appendingPathComponent("go/cmd/tetherdesktop/MenuBarIcon.png"))

// macOS: an iconset, then iconutil.
let iconset = fm.temporaryDirectory.appendingPathComponent("AppIcon-\(UUID().uuidString).iconset")
try fm.createDirectory(at: iconset, withIntermediateDirectories: true)
for base in [16, 32, 128, 256, 512] {
    try png(size: base).write(to: iconset.appendingPathComponent("icon_\(base)x\(base).png"))
    try png(size: base * 2).write(to: iconset.appendingPathComponent("icon_\(base)x\(base)@2x.png"))
}
let iconutil = Process()
iconutil.executableURL = URL(fileURLWithPath: "/usr/bin/iconutil")
iconutil.arguments = ["-c", "icns", iconset.path, "-o", assets.appendingPathComponent("AppIcon.icns").path]
try iconutil.run()
iconutil.waitUntilExit()
try? fm.removeItem(at: iconset)
guard iconutil.terminationStatus == 0 else { fatalError("iconutil failed") }

// Windows: an .ico with PNG entries, which Windows Vista and later read.
let sizes = [16, 20, 24, 32, 40, 48, 64, 256]
let images = sizes.map { png(size: $0) }
var ico = Data()
func u16(_ v: Int) { var x = UInt16(v).littleEndian; ico.append(Data(bytes: &x, count: 2)) }
func u32(_ v: Int) { var x = UInt32(v).littleEndian; ico.append(Data(bytes: &x, count: 4)) }
u16(0); u16(1); u16(sizes.count)
var offset = 6 + 16 * sizes.count
for (size, image) in zip(sizes, images) {
    ico.append(UInt8(size >= 256 ? 0 : size))
    ico.append(UInt8(size >= 256 ? 0 : size))
    ico.append(0); ico.append(0)
    u16(1); u16(32); u32(image.count); u32(offset)
    offset += image.count
}
images.forEach { ico.append($0) }
// The Go app: the tray icon it embeds, and the exe icon go-winres builds.
try ico.write(to: root.appendingPathComponent("go/cmd/tetherdesktop/AppIcon.ico"))
try png(size: 256).write(to: root.appendingPathComponent("go/cmd/tetherdesktop/winres/icon.png"))
print("wrote assets/AppIcon.icns, assets/AppIcon.png and the icons in go/cmd/tetherdesktop")
