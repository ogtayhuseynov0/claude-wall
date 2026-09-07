import Cocoa

// Renders the Claude Wall app icon (matches static/favicon.svg: dark rounded card
// with a 2×2 grid of panes, one live/orange with a sparkle) into ./AppIcon.iconset.

func drawLogo(_ px: CGFloat) -> NSBitmapImageRep {
    let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: Int(px), pixelsHigh: Int(px),
                               bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
                               colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)

    // scale from the 32-unit favicon grid; icon fills a little more of the canvas
    let pad: CGFloat = px * 0.06
    let g = (px - 2 * pad) / 32.0
    func R(_ x: CGFloat, _ y: CGFloat, _ w: CGFloat, _ h: CGFloat) -> NSRect {
        NSRect(x: pad + x * g, y: pad + (32 - (y + h)) * g, width: w * g, height: h * g) // flip y
    }

    let card = NSBezierPath(roundedRect: R(1, 1, 30, 30), xRadius: 7 * g, yRadius: 7 * g)
    NSGradient(starting: NSColor(srgbRed: 0.157, green: 0.169, blue: 0.212, alpha: 1),
               ending: NSColor(srgbRed: 0.102, green: 0.110, blue: 0.137, alpha: 1))?.draw(in: card, angle: -90)
    NSColor(srgbRed: 0.227, green: 0.247, blue: 0.302, alpha: 1).setStroke()
    card.lineWidth = 1 * g; card.stroke()

    let idle = NSColor(srgbRed: 0.184, green: 0.200, blue: 0.251, alpha: 1)
    for (x, y) in [(17.2, 5.6), (5.6, 17.2), (17.2, 17.2)] {
        idle.setFill()
        NSBezierPath(roundedRect: R(CGFloat(x), CGFloat(y), 9.2, 9.2), xRadius: 2.2 * g, yRadius: 2.2 * g).fill()
    }
    let live = NSBezierPath(roundedRect: R(5.6, 5.6, 9.2, 9.2), xRadius: 2.2 * g, yRadius: 2.2 * g)
    NSGradient(starting: NSColor(srgbRed: 0.961, green: 0.620, blue: 0.259, alpha: 1),
               ending: NSColor(srgbRed: 0.851, green: 0.463, blue: 0.024, alpha: 1))?.draw(in: live, angle: -45)

    // sparkle centered on the live pane
    let cx = pad + 10.2 * g, cy = pad + (32 - 10.2) * g, a = 3.7 * g, b = 1.0 * g
    let star = NSBezierPath()
    star.move(to: NSPoint(x: cx, y: cy + a))
    star.line(to: NSPoint(x: cx + b, y: cy + b)); star.line(to: NSPoint(x: cx + a, y: cy))
    star.line(to: NSPoint(x: cx + b, y: cy - b)); star.line(to: NSPoint(x: cx, y: cy - a))
    star.line(to: NSPoint(x: cx - b, y: cy - b)); star.line(to: NSPoint(x: cx - a, y: cy))
    star.line(to: NSPoint(x: cx - b, y: cy + b)); star.close()
    NSColor(srgbRed: 1, green: 0.965, blue: 0.925, alpha: 1).setFill(); star.fill()

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
