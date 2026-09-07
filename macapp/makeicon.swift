import Cocoa

// Renders the ClaudePiP app icon (blue→indigo rounded card + white picture-in-picture
// glyph) into ./AppIcon.iconset at every size iconutil needs.

func drawLogo(_ px: CGFloat) -> NSBitmapImageRep {
    let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: Int(px), pixelsHigh: Int(px),
                               bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
                               colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)

    let full = NSRect(x: 0, y: 0, width: px, height: px)
    let inset = px * 0.09
    let card = full.insetBy(dx: inset, dy: inset)
    let radius = px * 0.225
    let cardPath = NSBezierPath(roundedRect: card, xRadius: radius, yRadius: radius)
    let grad = NSGradient(starting: NSColor(srgbRed: 0.29, green: 0.48, blue: 0.98, alpha: 1),
                          ending:   NSColor(srgbRed: 0.44, green: 0.36, blue: 0.96, alpha: 1))
    grad?.draw(in: cardPath, angle: -60)

    // PiP glyph: outer rounded-rect "screen" outline + filled inner window bottom-right
    let g = card.insetBy(dx: card.width * 0.22, dy: card.height * 0.24)
    let outer = NSBezierPath(roundedRect: g, xRadius: px * 0.045, yRadius: px * 0.045)
    outer.lineWidth = px * 0.036
    NSColor.white.setStroke()
    outer.stroke()

    let pw = g.width * 0.44, ph = g.height * 0.44
    let pip = NSRect(x: g.maxX - pw - g.width * 0.055,
                     y: g.minY + g.height * 0.055, width: pw, height: ph)
    let pipPath = NSBezierPath(roundedRect: pip, xRadius: px * 0.028, yRadius: px * 0.028)
    NSColor.white.setFill()
    pipPath.fill()

    NSGraphicsContext.restoreGraphicsState()
    return rep
}

func savePNG(_ rep: NSBitmapImageRep, _ path: String) {
    guard let data = rep.representation(using: .png, properties: [:]) else { return }
    try? data.write(to: URL(fileURLWithPath: path))
}

let dir = "AppIcon.iconset"
try? FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)

// (filename, pixel size)
let items: [(String, CGFloat)] = [
    ("icon_16x16.png", 16), ("icon_16x16@2x.png", 32),
    ("icon_32x32.png", 32), ("icon_32x32@2x.png", 64),
    ("icon_128x128.png", 128), ("icon_128x128@2x.png", 256),
    ("icon_256x256.png", 256), ("icon_256x256@2x.png", 512),
    ("icon_512x512.png", 512), ("icon_512x512@2x.png", 1024),
]
var cache: [CGFloat: NSBitmapImageRep] = [:]
for (name, px) in items {
    let rep = cache[px] ?? drawLogo(px)
    cache[px] = rep
    savePNG(rep, "\(dir)/\(name)")
}
print("wrote \(dir)")
