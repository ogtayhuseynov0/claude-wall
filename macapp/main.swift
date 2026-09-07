import Cocoa
import WebKit
import Carbon.HIToolbox

let WALL = "http://127.0.0.1:7685"

// The claude-wall logo (matches static/favicon.svg): a rounded dark card with a
// 2×2 grid of panes — three idle (gray) + one live (Claude orange) with a sparkle.
// Colored (not a template) so it reads the same in the menubar, popover, and dock.
func wallLogo(_ px: CGFloat) -> NSImage {
    let s = px / 32.0
    // svg coords are top-left origin; AppKit is bottom-up — flip y.
    func R(_ x: CGFloat, _ y: CGFloat, _ w: CGFloat, _ h: CGFloat) -> NSRect {
        NSRect(x: x * s, y: px - (y + h) * s, width: w * s, height: h * s)
    }
    let img = NSImage(size: NSSize(width: px, height: px))
    img.lockFocus()

    // card body with vertical gradient
    let card = NSBezierPath(roundedRect: R(1, 1, 30, 30), xRadius: 7 * s, yRadius: 7 * s)
    NSGradient(starting: NSColor(srgbRed: 0.157, green: 0.169, blue: 0.212, alpha: 1),
               ending: NSColor(srgbRed: 0.102, green: 0.110, blue: 0.137, alpha: 1))?
        .draw(in: card, angle: -90)
    NSColor(srgbRed: 0.227, green: 0.247, blue: 0.302, alpha: 1).setStroke()
    card.lineWidth = 1 * s; card.stroke()

    let idle = NSColor(srgbRed: 0.184, green: 0.200, blue: 0.251, alpha: 1)
    for (x, y) in [(17.2, 5.6), (5.6, 17.2), (17.2, 17.2)] {
        idle.setFill()
        NSBezierPath(roundedRect: R(CGFloat(x), CGFloat(y), 9.2, 9.2), xRadius: 2.2 * s, yRadius: 2.2 * s).fill()
    }
    // live pane (orange gradient)
    let live = NSBezierPath(roundedRect: R(5.6, 5.6, 9.2, 9.2), xRadius: 2.2 * s, yRadius: 2.2 * s)
    NSGradient(starting: NSColor(srgbRed: 0.961, green: 0.620, blue: 0.259, alpha: 1),
               ending: NSColor(srgbRed: 0.851, green: 0.463, blue: 0.024, alpha: 1))?
        .draw(in: live, angle: -45)
    // sparkle on the live pane (only legible at larger sizes)
    if px >= 24 {
        let cx = 10.2 * s, cy = px - 10.2 * s, a = 3.7 * s, b = 1.0 * s
        let star = NSBezierPath()
        star.move(to: NSPoint(x: cx, y: cy + a))
        star.line(to: NSPoint(x: cx + b, y: cy + b))
        star.line(to: NSPoint(x: cx + a, y: cy))
        star.line(to: NSPoint(x: cx + b, y: cy - b))
        star.line(to: NSPoint(x: cx, y: cy - a))
        star.line(to: NSPoint(x: cx - b, y: cy - b))
        star.line(to: NSPoint(x: cx - a, y: cy))
        star.line(to: NSPoint(x: cx - b, y: cy + b))
        star.close()
        NSColor(srgbRed: 1, green: 0.965, blue: 0.925, alpha: 1).setFill(); star.fill()
    }

    img.unlockFocus()
    return img
}
func menubarGlyph() -> NSImage { wallLogo(18) }

struct Pane: Decodable {
    let target: String
    let session: String
    let dirName: String
    let agent: String
    let branch: String
}

struct SummaryPane: Decodable { let target: String; let status: String }
struct Summary: Decodable {
    let total: Int
    let working: Int
    let pending: Int
    let stale: Int?
    let panes: [SummaryPane]
}

// finance: rolling usage windows per account + per-repo weekly spend
struct FinBucket: Decodable { let name: String; let cost: Double }
struct UsageWindow: Decodable {
    let today: Double
    let week: Double
    let window5h: Double
    let byRepoWeek: [FinBucket]?
}
struct UsageCaps: Decodable { let window5h: Double; let week: Double }
struct Usage: Decodable {
    let personal: UsageWindow
    let work: UsageWindow?
    let caps: UsageCaps
    let workCaps: UsageCaps?
}

// real subscription usage from Claude's /api/oauth/usage (utilization 0..1 or 0..100)
struct LimWin: Decodable { let utilization: Double }
struct AccountLimits: Decodable { let five_hour: LimWin; let seven_day: LimWin }
struct Limits: Decodable { let personal: AccountLimits?; let work: AccountLimits? }

// tint a template image a solid color
func tinted(_ image: NSImage, _ color: NSColor) -> NSImage {
    let out = NSImage(size: image.size)
    out.lockFocus()
    color.set()
    let r = NSRect(origin: .zero, size: image.size)
    image.draw(in: r)
    r.fill(using: .sourceAtop)
    out.unlockFocus()
    out.isTemplate = false
    return out
}

func statusColor(_ s: String) -> NSColor {
    switch s {
    case "working":    return NSColor(calibratedRed: 0.28, green: 0.74, blue: 0.44, alpha: 1)
    case "permission": return NSColor(calibratedRed: 0.92, green: 0.30, blue: 0.34, alpha: 1)
    case "stale":      return NSColor(calibratedRed: 0.96, green: 0.74, blue: 0.18, alpha: 1) // idle >30m
    case "error":      return NSColor(calibratedRed: 0.95, green: 0.60, blue: 0.20, alpha: 1)
    default:           return NSColor(calibratedWhite: 0.55, alpha: 1) // idle
    }
}

// ── Floating, draggable, always-on-top button ───────────────────────────────
final class ButtonView: NSView {
    var onClick: (() -> Void)?
    var onRightClick: (() -> Void)?
    private var startInWindow: NSPoint = .zero
    private var moved = false

    var idle = 0, working = 0, pending = 0, stale = 0
    private lazy var logo: NSImage = wallLogo(48)

    func setStatus(idle: Int, working: Int, pending: Int, stale: Int) {
        if self.idle == idle && self.working == working && self.pending == pending && self.stale == stale { return }
        self.idle = idle; self.working = working; self.pending = pending; self.stale = stale
        needsDisplay = true
    }

    private func badge(_ n: Int, at p: NSPoint, color: NSColor) {
        guard n > 0 else { return }
        let d: CGFloat = 19
        let rect = NSRect(x: p.x, y: p.y, width: d, height: d)
        let path = NSBezierPath(ovalIn: rect)
        color.setFill(); path.fill()
        NSColor.white.withAlphaComponent(0.9).setStroke(); path.lineWidth = 1.4; path.stroke()
        let s = "\(n)" as NSString
        let a: [NSAttributedString.Key: Any] = [
            .font: NSFont.boldSystemFont(ofSize: 11),
            .foregroundColor: NSColor.white,
        ]
        let sz = s.size(withAttributes: a)
        s.draw(at: NSPoint(x: rect.minX + (d - sz.width) / 2, y: rect.minY + (d - sz.height) / 2), withAttributes: a)
    }

    override func draw(_ dirtyRect: NSRect) {
        // the claude-wall logo IS the button face
        logo.draw(in: bounds.insetBy(dx: 3, dy: 3), from: .zero, operation: .sourceOver, fraction: 1)

        let d: CGFloat = 19, pad: CGFloat = 1
        // idle → top-left, working → top-right, permission → bottom-right, stale(>30m) → bottom-left
        badge(idle,    at: NSPoint(x: pad, y: bounds.maxY - d - pad), color: statusColor("idle"))
        badge(working, at: NSPoint(x: bounds.maxX - d - pad, y: bounds.maxY - d - pad), color: statusColor("working"))
        badge(pending, at: NSPoint(x: bounds.maxX - d - pad, y: pad), color: statusColor("permission"))
        badge(stale,   at: NSPoint(x: pad, y: pad), color: statusColor("stale"))
    }
    // register clicks even when the app/window isn't focused (no double-click needed)
    override func acceptsFirstMouse(for event: NSEvent?) -> Bool { true }
    override func mouseDown(with e: NSEvent) { startInWindow = e.locationInWindow; moved = false }
    override func mouseDragged(with e: NSEvent) {
        moved = true
        guard let w = window else { return }
        let m = NSEvent.mouseLocation
        w.setFrameOrigin(NSPoint(x: m.x - startInWindow.x, y: m.y - startInWindow.y))
    }
    override func mouseUp(with e: NSEvent) { if !moved { onClick?() } }
    override func rightMouseDown(with e: NSEvent) { onRightClick?() }
}

final class ButtonWindow: NSPanel {
    override var canBecomeKey: Bool { false } // never steal focus from the active app
    init() {
        super.init(contentRect: NSRect(x: 0, y: 0, width: 58, height: 58),
                   styleMask: [.borderless, .nonactivatingPanel], backing: .buffered, defer: false)
        isFloatingPanel = true
        isOpaque = false
        backgroundColor = .clear
        level = .floating
        collectionBehavior = [.canJoinAllSpaces, .stationary, .fullScreenAuxiliary]
        hasShadow = true
        let v = ButtonView(frame: NSRect(x: 0, y: 0, width: 58, height: 58))
        v.autoresizingMask = [.width, .height]
        contentView = v
        if let scr = NSScreen.main {
            let f = scr.visibleFrame
            setFrameOrigin(NSPoint(x: f.maxX - 78, y: f.maxY - 78))
        }
    }
    var buttonView: ButtonView { contentView as! ButtonView }
}

// ── Spotlight-style searchable pane picker ──────────────────────────────────
final class PickerPanel: NSPanel {
    override var canBecomeKey: Bool { true }
    override var canBecomeMain: Bool { true }
}

final class PickerController: NSObject, NSTableViewDataSource, NSTableViewDelegate, NSTextFieldDelegate, NSWindowDelegate {
    let panel: PickerPanel
    let search = NSTextField()
    let table = NSTableView()
    let filterSeg = NSSegmentedControl(labels: ["All", "Working", "Perm", "30m+", "Idle"],
                                       trackingMode: .selectOne, target: nil, action: nil)
    let sortSeg = NSSegmentedControl(labels: ["Sort: Status", "Name"],
                                     trackingMode: .selectOne, target: nil, action: nil)
    var all: [(pane: Pane, status: String)] = []
    var rows: [(pane: Pane, status: String)] = []
    var onPick: ((String, String) -> Void)?
    var onOpenAll: (([(target: String, name: String)]) -> Void)?
    var clickMonitor: Any?

    private var statusFilter: String? {
        switch filterSeg.selectedSegment {
        case 1: return "working"
        case 2: return "permission"
        case 3: return "stale"
        case 4: return "idle"
        default: return nil
        }
    }
    private var sortByName: Bool { sortSeg.selectedSegment == 1 }

    override init() {
        let W: CGFloat = 480, H: CGFloat = 508
        panel = PickerPanel(contentRect: NSRect(x: 0, y: 0, width: W, height: H),
                            styleMask: [.borderless],
                            backing: .buffered, defer: false)
        super.init()
        panel.isOpaque = false
        panel.backgroundColor = .clear
        panel.hasShadow = true
        panel.level = .floating
        panel.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary]
        panel.isMovableByWindowBackground = true
        panel.hidesOnDeactivate = false
        panel.delegate = self

        let bg = NSVisualEffectView(frame: NSRect(x: 0, y: 0, width: W, height: H))
        bg.autoresizingMask = [.width, .height]
        bg.material = .sidebar
        bg.blendingMode = .behindWindow
        bg.state = .active
        bg.wantsLayer = true
        bg.layer?.cornerRadius = 14
        bg.layer?.masksToBounds = true
        panel.contentView = bg

        // header logo + title
        let logo = NSImageView(frame: NSRect(x: 16, y: H - 42, width: 22, height: 22))
        if let s = NSImage(systemSymbolName: "pip.fill", accessibilityDescription: nil) {
            logo.image = s
            logo.contentTintColor = NSColor(calibratedRed: 0.45, green: 0.55, blue: 0.97, alpha: 1)
        }
        bg.addSubview(logo)

        // search field
        search.frame = NSRect(x: 44, y: H - 46, width: W - 60, height: 30)
        search.placeholderString = "Search panes…"
        search.font = NSFont.systemFont(ofSize: 16)
        search.isBordered = false
        search.drawsBackground = false
        search.focusRingType = .none
        search.delegate = self
        search.autoresizingMask = [.width]
        bg.addSubview(search)

        let sep = NSBox(frame: NSRect(x: 0, y: H - 54, width: W, height: 1))
        sep.boxType = .separator
        sep.autoresizingMask = [.width]
        bg.addSubview(sep)

        // status filter + sort controls
        filterSeg.frame = NSRect(x: 10, y: H - 88, width: W - 20, height: 24)
        filterSeg.selectedSegment = 0
        filterSeg.segmentDistribution = .fillEqually
        filterSeg.controlSize = .small
        filterSeg.target = self; filterSeg.action = #selector(controlsChanged)
        filterSeg.autoresizingMask = [.width]
        bg.addSubview(filterSeg)

        sortSeg.frame = NSRect(x: 10, y: H - 118, width: 200, height: 22)
        sortSeg.selectedSegment = 0
        sortSeg.controlSize = .small
        sortSeg.target = self; sortSeg.action = #selector(controlsChanged)
        bg.addSubview(sortSeg)

        let sep2 = NSBox(frame: NSRect(x: 0, y: H - 128, width: W, height: 1))
        sep2.boxType = .separator
        sep2.autoresizingMask = [.width]
        bg.addSubview(sep2)

        // footer: open everything currently listed, tiled (⌘↩)
        let footer = NSButton(title: "Open all listed  (⌘↩)", target: self, action: #selector(openAllListed))
        footer.bezelStyle = .rounded
        footer.keyEquivalent = "\r"
        footer.keyEquivalentModifierMask = .command
        footer.frame = NSRect(x: 10, y: 10, width: W - 20, height: 28)
        footer.autoresizingMask = [.width]
        bg.addSubview(footer)

        // table in scroll view — inset from the rounded panel edges so rows never
        // bleed under the corners / header separator
        let scroll = NSScrollView(frame: NSRect(x: 8, y: 46, width: W - 16, height: H - 176))
        scroll.autoresizingMask = [.width, .height]
        scroll.hasVerticalScroller = true
        scroll.drawsBackground = false
        scroll.borderType = .noBorder
        scroll.automaticallyAdjustsContentInsets = false
        scroll.contentInsets = NSEdgeInsets(top: 4, left: 0, bottom: 4, right: 0)

        let col = NSTableColumn(identifier: NSUserInterfaceItemIdentifier("c"))
        col.width = W - 16
        table.addTableColumn(col)
        table.headerView = nil
        table.backgroundColor = .clear
        table.rowHeight = 46
        table.style = .inset   // padded, rounded selection that stays inside the panel
        table.selectionHighlightStyle = .regular
        table.dataSource = self
        table.delegate = self
        table.target = self
        table.doubleAction = #selector(openSelected)
        table.intercellSpacing = NSSize(width: 0, height: 2)
        scroll.documentView = table
        bg.addSubview(scroll)
    }

    // permission first, then working, then error, then idle; alphabetical within a group
    private func rank(_ s: String) -> Int {
        switch s {
        case "permission": return 0
        case "stale": return 1        // idle >30m — surface for attention
        case "working": return 2
        case "error": return 3
        default: return 4             // idle
        }
    }
    private func name(_ p: Pane) -> String { p.dirName.isEmpty ? p.session : p.dirName }
    private func sorted(_ items: [(pane: Pane, status: String)]) -> [(pane: Pane, status: String)] {
        items.sorted {
            if !sortByName {
                let ra = rank($0.status), rb = rank($1.status)
                if ra != rb { return ra < rb }
            }
            return name($0.pane).localizedCaseInsensitiveCompare(name($1.pane)) == .orderedAscending
        }
    }

    @objc private func controlsChanged() { applyFilter(search.stringValue); selectRow(0) }

    func show(_ items: [(pane: Pane, status: String)], onPick: @escaping (String, String) -> Void) {
        self.onPick = onPick
        self.all = items
        search.stringValue = ""
        applyFilter("")
        // center upper-third on the active screen
        if let scr = NSScreen.main {
            let f = scr.visibleFrame
            let s = panel.frame.size
            panel.setFrameOrigin(NSPoint(x: f.midX - s.width / 2, y: f.midY - s.height / 2 + f.height * 0.10))
        }
        selectRow(0)
        NSApp.activate(ignoringOtherApps: true)
        panel.makeKeyAndOrderFront(nil)
        panel.initialFirstResponder = search
        // focus the search field on the next runloop tick (reliable once the panel is key)
        DispatchQueue.main.async {
            self.panel.makeFirstResponder(self.search)
            self.search.currentEditor()?.selectAll(nil)
        }
        // close when a click lands outside the panel (in any other app/window)
        if clickMonitor == nil {
            clickMonitor = NSEvent.addGlobalMonitorForEvents(matching: [.leftMouseDown, .rightMouseDown]) { [weak self] _ in
                self?.hide()
            }
        }
    }

    // clicking another window resigns key → dismiss
    func windowDidResignKey(_ notification: Notification) { hide() }

    func refreshStatuses(_ map: [String: String]) {
        guard panel.isVisible else { return }
        all = sorted(all.map { ($0.pane, map[$0.pane.target] ?? $0.status) })
        applyFilter(search.stringValue)
    }

    func hide() {
        if let m = clickMonitor { NSEvent.removeMonitor(m); clickMonitor = nil }
        panel.orderOut(nil)
    }

    private func applyFilter(_ q: String) {
        let sel = selectedTarget()
        let t = q.trimmingCharacters(in: .whitespaces).lowercased()
        let sf = statusFilter
        rows = sorted(all.filter {
            if let sf = sf, $0.status != sf { return false }
            if t.isEmpty { return true }
            let hay = "\($0.pane.dirName) \($0.pane.session) \($0.pane.target) \($0.pane.agent) \($0.pane.branch)".lowercased()
            return hay.contains(t)
        })
        table.reloadData()
        // keep selection on the same target if still present, else first row
        if let sel = sel, let i = rows.firstIndex(where: { $0.pane.target == sel }) { selectRow(i) }
        else { selectRow(0) }
    }

    private func selectedTarget() -> String? {
        let r = table.selectedRow
        return (r >= 0 && r < rows.count) ? rows[r].pane.target : nil
    }

    private func selectRow(_ i: Int) {
        guard !rows.isEmpty else { return }
        let idx = max(0, min(i, rows.count - 1))
        table.selectRowIndexes(IndexSet(integer: idx), byExtendingSelection: false)
        table.scrollRowToVisible(idx)
    }

    @objc func openSelected() {
        let r = table.selectedRow
        guard r >= 0 && r < rows.count else { return }
        let p = rows[r].pane
        let name = p.dirName.isEmpty ? p.session : p.dirName
        hide()
        onPick?(p.target, name)
    }

    @objc func openAllListed() {
        let items = rows.map { (target: $0.pane.target, name: $0.pane.dirName.isEmpty ? $0.pane.session : $0.pane.dirName) }
        guard !items.isEmpty else { return }
        hide()
        onOpenAll?(items)
    }

    // NSTableView
    func numberOfRows(in tableView: NSTableView) -> Int { rows.count }

    func tableView(_ tv: NSTableView, viewFor col: NSTableColumn?, row: Int) -> NSView? {
        let p = rows[row].pane
        let st = rows[row].status
        let cell = NSView()

        let dot = NSView()
        dot.translatesAutoresizingMaskIntoConstraints = false
        dot.wantsLayer = true
        dot.layer?.cornerRadius = 4.5
        dot.layer?.backgroundColor = statusColor(st).cgColor

        let name = NSTextField(labelWithString: p.dirName.isEmpty ? p.session : p.dirName)
        name.translatesAutoresizingMaskIntoConstraints = false
        name.font = NSFont.systemFont(ofSize: 14, weight: .semibold)
        name.lineBreakMode = .byTruncatingTail

        let sub = NSTextField(labelWithString: "\(p.agent)  ·  \(p.target)\(p.branch.isEmpty ? "" : "  ·  " + p.branch)")
        sub.translatesAutoresizingMaskIntoConstraints = false
        sub.font = NSFont.systemFont(ofSize: 11)
        sub.textColor = .secondaryLabelColor
        sub.lineBreakMode = .byTruncatingTail

        let badge = NSTextField(labelWithString: st)
        badge.translatesAutoresizingMaskIntoConstraints = false
        badge.font = NSFont.systemFont(ofSize: 10, weight: .semibold)
        badge.textColor = statusColor(st)
        badge.alignment = .right
        badge.setContentHuggingPriority(.required, for: .horizontal)
        badge.setContentCompressionResistancePriority(.required, for: .horizontal)

        [dot, name, sub, badge].forEach { cell.addSubview($0) }
        NSLayoutConstraint.activate([
            dot.leadingAnchor.constraint(equalTo: cell.leadingAnchor, constant: 6),
            dot.centerYAnchor.constraint(equalTo: cell.centerYAnchor),
            dot.widthAnchor.constraint(equalToConstant: 9),
            dot.heightAnchor.constraint(equalToConstant: 9),

            name.leadingAnchor.constraint(equalTo: dot.trailingAnchor, constant: 12),
            name.topAnchor.constraint(equalTo: cell.topAnchor, constant: 6),
            name.trailingAnchor.constraint(lessThanOrEqualTo: badge.leadingAnchor, constant: -10),

            sub.leadingAnchor.constraint(equalTo: name.leadingAnchor),
            sub.topAnchor.constraint(equalTo: name.bottomAnchor, constant: 2),
            sub.trailingAnchor.constraint(lessThanOrEqualTo: badge.leadingAnchor, constant: -10),

            badge.trailingAnchor.constraint(equalTo: cell.trailingAnchor, constant: -14),
            badge.centerYAnchor.constraint(equalTo: cell.centerYAnchor),
        ])
        return cell
    }

    // NSTextField (search) keyboard routing
    func controlTextDidChange(_ obj: Notification) { applyFilter(search.stringValue) }

    func control(_ control: NSControl, textView: NSTextView, doCommandBy sel: Selector) -> Bool {
        switch sel {
        case #selector(NSResponder.moveDown(_:)):     selectRow(table.selectedRow + 1); return true
        case #selector(NSResponder.moveUp(_:)):       selectRow(table.selectedRow - 1); return true
        case #selector(NSResponder.insertNewline(_:)): openSelected(); return true
        case #selector(NSResponder.cancelOperation(_:)): hide(); return true
        default: return false
        }
    }
}

// money formatting: $12.34, $1.2k
func fmtMoney(_ v: Double) -> String {
    if v >= 1000 { return String(format: "$%.1fk", v / 1000) }
    return String(format: "$%.2f", v)
}

// ── Custom menubar popover: spending + counts + transparency + actions ──────
final class StatusPanel: NSViewController {
    var onOpen: (() -> Void)?
    var onOpenActive: (() -> Void)?
    var onOpenDashboard: (() -> Void)?
    var onRestart: (() -> Void)?
    var onToggleButton: (() -> Void)?
    var onCloseAll: (() -> Void)?
    var onQuit: (() -> Void)?
    var onAlpha: ((Double) -> Void)?

    private let workLabel = StatusPanel.countLabel()
    private let permLabel = StatusPanel.countLabel()
    private let staleLabel = StatusPanel.countLabel()
    private let idleLabel = StatusPanel.countLabel()
    private let totalLabel = NSTextField(labelWithString: "0 panes")
    private let slider = NSSlider()
    private let pctLabel = NSTextField(labelWithString: "90%")
    private let usageBox = NSStackView()
    private let spendersBox = NSStackView()

    static func countLabel() -> NSTextField {
        let l = NSTextField(labelWithString: "0")
        l.font = NSFont.systemFont(ofSize: 13, weight: .semibold)
        return l
    }

    private func dot(_ color: NSColor) -> NSView {
        let v = NSView(); v.translatesAutoresizingMaskIntoConstraints = false
        v.wantsLayer = true; v.layer?.cornerRadius = 4
        v.layer?.backgroundColor = color.cgColor
        v.widthAnchor.constraint(equalToConstant: 8).isActive = true
        v.heightAnchor.constraint(equalToConstant: 8).isActive = true
        return v
    }

    private func countGroup(_ color: NSColor, _ label: NSTextField, _ caption: String) -> NSView {
        let cap = NSTextField(labelWithString: caption)
        cap.font = NSFont.systemFont(ofSize: 10); cap.textColor = .secondaryLabelColor
        let top = NSStackView(views: [dot(color), label])
        top.spacing = 5; top.alignment = .centerY
        let col = NSStackView(views: [top, cap])
        col.orientation = .vertical; col.spacing = 1; col.alignment = .leading
        return col
    }

    private func cap(_ s: String) -> NSTextField {
        let l = NSTextField(labelWithString: s)
        l.font = NSFont.systemFont(ofSize: 10, weight: .medium)
        l.textColor = .secondaryLabelColor
        return l
    }

    override func loadView() {
        let W: CGFloat = 320, cw: CGFloat = 292
        let bg = NSVisualEffectView(frame: NSRect(x: 0, y: 0, width: W, height: 552))
        bg.material = .popover; bg.blendingMode = .behindWindow; bg.state = .active

        // header
        let logo = NSImageView()
        logo.image = wallLogo(20)
        logo.translatesAutoresizingMaskIntoConstraints = false
        logo.widthAnchor.constraint(equalToConstant: 20).isActive = true
        logo.heightAnchor.constraint(equalToConstant: 20).isActive = true
        let titleL = NSTextField(labelWithString: "Claude Wall")
        titleL.font = NSFont.systemFont(ofSize: 14, weight: .bold)
        let header = NSStackView(views: [logo, titleL, NSView(), totalLabel])
        totalLabel.font = NSFont.systemFont(ofSize: 11); totalLabel.textColor = .secondaryLabelColor
        header.spacing = 8; header.alignment = .centerY

        // usage windows (5h · week) per account
        usageBox.orientation = .vertical; usageBox.spacing = 4; usageBox.alignment = .leading
        let usageCol = NSStackView(views: [cap("USAGE  —  5H · WEEK"), usageBox])
        usageCol.orientation = .vertical; usageCol.spacing = 4; usageCol.alignment = .leading

        // top repos today
        spendersBox.orientation = .vertical; spendersBox.spacing = 3; spendersBox.alignment = .leading
        let spendersCol = NSStackView(views: [cap("TOP REPOS — WEEK"), spendersBox])
        spendersCol.orientation = .vertical; spendersCol.spacing = 4; spendersCol.alignment = .leading

        // counts row
        let counts = NSStackView(views: [
            countGroup(statusColor("working"), workLabel, "working"),
            countGroup(statusColor("permission"), permLabel, "perm"),
            countGroup(statusColor("stale"), staleLabel, "idle 30m+"),
            countGroup(statusColor("idle"), idleLabel, "idle"),
        ])
        counts.distribution = .fillEqually; counts.alignment = .top

        // transparency slider
        pctLabel.font = NSFont.systemFont(ofSize: 11, weight: .medium)
        pctLabel.alignment = .right
        let transHdr = NSStackView(views: [cap("PIP TRANSPARENCY"), NSView(), pctLabel])
        transHdr.alignment = .centerY
        slider.minValue = 0.2; slider.maxValue = 1.0; slider.doubleValue = 0.9
        slider.target = self; slider.action = #selector(sliderMoved)
        slider.isContinuous = true

        // action buttons
        let open = row("Open PiP…", "⌘⌥P", #selector(tapOpen))
        let active = row("Open working + waiting (tiled)", "", #selector(tapActive))
        let dash = row("Open dashboard", "", #selector(tapDashboard))
        let restart = row("Restart server", "", #selector(tapRestart))
        let toggle = row("Show / Hide floating button", "", #selector(tapToggle))
        let closeAll = row("Close all PiPs", "", #selector(tapClose))
        let quit = row("Quit Claude Wall", "⌘Q", #selector(tapQuit))

        let stack = NSStackView(views: [
            header, sep(), usageCol, sep(), spendersCol, sep(), counts, sep(),
            transHdr, slider, sep(), open, active, dash, restart, toggle, closeAll, quit,
        ])
        stack.orientation = .vertical
        stack.spacing = 9
        stack.alignment = .leading
        stack.edgeInsets = NSEdgeInsets(top: 14, left: 14, bottom: 12, right: 14)
        stack.translatesAutoresizingMaskIntoConstraints = false
        bg.addSubview(stack)
        NSLayoutConstraint.activate([
            stack.leadingAnchor.constraint(equalTo: bg.leadingAnchor),
            stack.trailingAnchor.constraint(equalTo: bg.trailingAnchor),
            stack.topAnchor.constraint(equalTo: bg.topAnchor),
            stack.bottomAnchor.constraint(equalTo: bg.bottomAnchor),
        ])
        for v in [header, usageCol, spendersCol, spendersBox, counts, transHdr, slider, open, active, dash, restart, toggle, closeAll, quit] {
            v.translatesAutoresizingMaskIntoConstraints = false
            v.widthAnchor.constraint(equalToConstant: cw).isActive = true
        }
        self.view = bg
    }

    private func sep() -> NSView {
        let b = NSBox(); b.boxType = .separator
        b.translatesAutoresizingMaskIntoConstraints = false
        b.heightAnchor.constraint(equalToConstant: 1).isActive = true
        return b
    }

    private func row(_ title: String, _ key: String, _ action: Selector) -> NSButton {
        let b = NSButton(title: key.isEmpty ? title : "\(title)   \(key)", target: self, action: action)
        b.bezelStyle = .inline
        b.isBordered = false
        b.contentTintColor = .labelColor
        b.alignment = .left
        b.font = NSFont.systemFont(ofSize: 13)
        b.heightAnchor.constraint(equalToConstant: 22).isActive = true
        return b
    }

    @objc private func sliderMoved() { pctLabel.stringValue = "\(Int(slider.doubleValue * 100))%"; onAlpha?(slider.doubleValue) }
    @objc private func tapOpen() { onOpen?() }
    @objc private func tapActive() { onOpenActive?() }
    @objc private func tapDashboard() { onOpenDashboard?() }
    @objc private func tapRestart() { onRestart?() }
    @objc private func tapToggle() { onToggleButton?() }
    @objc private func tapClose() { onCloseAll?() }
    @objc private func tapQuit() { onQuit?() }

    func setCounts(working: Int, pending: Int, stale: Int, idle: Int, total: Int) {
        workLabel.stringValue = "\(working)"
        permLabel.stringValue = "\(pending)"
        staleLabel.stringValue = "\(stale)"
        idleLabel.stringValue = "\(idle)"
        totalLabel.stringValue = "\(total) panes"
    }
    func setAlpha(_ v: Double) { slider.doubleValue = v; pctLabel.stringValue = "\(Int(v * 100))%" }

    private func pct(_ cost: Double, _ cap: Double) -> Int {
        guard cap > 0 else { return 0 }
        return min(999, Int((cost / cap * 100).rounded()))
    }
    private func utilPct(_ u: Double) -> Int { min(999, Int((u <= 1.0 ? u * 100 : u).rounded())) }

    // Prefers real subscription utilization (from Claude's /usage) when available;
    // otherwise falls back to spend as a % of the configured cap.
    private func usageRow(_ name: String, _ u: UsageWindow, _ caps: UsageCaps, _ real: AccountLimits?) -> NSView {
        let n = NSTextField(labelWithString: name)
        n.font = NSFont.systemFont(ofSize: 12, weight: .medium)
        let p5: Int, pw: Int, tip: String
        if let real = real {
            p5 = utilPct(real.five_hour.utilization); pw = utilPct(real.seven_day.utilization)
            tip = "live from Claude · 5h \(p5)% · week \(pw)%"
        } else {
            p5 = pct(u.window5h, caps.window5h); pw = pct(u.week, caps.week)
            tip = "estimate · 5h \(fmtMoney(u.window5h)) of \(fmtMoney(caps.window5h))  ·  week \(fmtMoney(u.week)) of \(fmtMoney(caps.week))"
        }
        let v = NSTextField(labelWithString: "\(p5)%  ·  \(pw)%")
        v.font = NSFont.monospacedDigitSystemFont(ofSize: 12, weight: .medium)
        v.alignment = .right
        v.textColor = (p5 >= 90 || pw >= 90) ? statusColor("permission") : .labelColor
        v.setContentHuggingPriority(.required, for: .horizontal)
        let r = NSStackView(views: [n, NSView(), v])
        r.alignment = .centerY
        r.toolTip = tip
        r.translatesAutoresizingMaskIntoConstraints = false
        r.widthAnchor.constraint(equalToConstant: 292).isActive = true
        return r
    }

    func setUsage(_ u: Usage, _ limits: Limits?) {
        usageBox.arrangedSubviews.forEach { $0.removeFromSuperview() }
        usageBox.addArrangedSubview(usageRow("Personal", u.personal, u.caps, limits?.personal))
        if let w = u.work { usageBox.addArrangedSubview(usageRow("Work", w, u.workCaps ?? u.caps, limits?.work)) }
    }

    // Split each account's weekly utilization across its repos by spend share:
    // repo% ≈ accountWeeklyUtilization × (repo weekly spend / account weekly spend).
    // Approximate — Claude only reports the account-level number.
    func setRepoUsage(_ u: Usage, _ limits: Limits?) {
        spendersBox.arrangedSubviews.forEach { $0.removeFromSuperview() }
        var items: [(name: String, pct: Double, cost: Double, hasPct: Bool)] = []
        func add(_ w: UsageWindow?, _ lim: AccountLimits?) {
            guard let w = w, let repos = w.byRepoWeek, w.week > 0 else { return }
            let util = lim.map { $0.seven_day.utilization <= 1 ? $0.seven_day.utilization * 100 : $0.seven_day.utilization }
            for r in repos {
                let p = (util ?? 0) * r.cost / w.week
                items.append((r.name, p, r.cost, util != nil))
            }
        }
        add(u.personal, limits?.personal)
        add(u.work, limits?.work)
        items.sort { $0.cost > $1.cost }

        if items.isEmpty {
            let l = NSTextField(labelWithString: "no spend recorded this week")
            l.font = NSFont.systemFont(ofSize: 11); l.textColor = .tertiaryLabelColor
            spendersBox.addArrangedSubview(l)
            return
        }
        for s in items.prefix(5) {
            let n = NSTextField(labelWithString: s.name)
            n.font = NSFont.systemFont(ofSize: 12); n.lineBreakMode = .byTruncatingTail
            let c = NSTextField(labelWithString: s.hasPct ? String(format: "%.1f%%", s.pct) : fmtMoney(s.cost))
            c.font = NSFont.monospacedDigitSystemFont(ofSize: 12, weight: .medium)
            c.alignment = .right
            c.setContentHuggingPriority(.required, for: .horizontal)
            let r = NSStackView(views: [n, NSView(), c])
            r.alignment = .centerY
            r.toolTip = "\(fmtMoney(s.cost)) this week" + (s.hasPct ? "  ·  ~\(String(format: "%.1f", s.pct))% of weekly usage" : "")
            r.translatesAutoresizingMaskIntoConstraints = false
            r.widthAnchor.constraint(equalToConstant: 292).isActive = true
            spendersBox.addArrangedSubview(r)
        }
    }
}

// ── App ─────────────────────────────────────────────────────────────────────
final class AppDelegate: NSObject, NSApplicationDelegate, NSWindowDelegate, WKNavigationDelegate {
    var statusItem: NSStatusItem!
    var buttonWindow: ButtonWindow!
    var pips: [String: NSWindow] = [:]
    var pipBgs: [String: NSView] = [:]   // native dark backdrop per PiP (alpha = transparency)
    var pollTimer: Timer?
    var statusByTarget: [String: String] = [:]
    let picker = PickerController()
    var spinners: [ObjectIdentifier: NSProgressIndicator] = [:]
    var hotKeyRef: EventHotKeyRef?
    var pipAlpha: CGFloat = 0.9
    lazy var statusImage: NSImage = menubarGlyph()   // build once, reuse (recreating collapsed the item)
    let statusPanel = StatusPanel()
    let popover = NSPopover()
    var serverProcess: Process?   // the claude-wall server we spawned (nil if we reused a running one)
    var lastUsage: Usage?
    var lastLimits: Limits?

    func applicationDidFinishLaunching(_ note: Notification) {
        NSApp.setActivationPolicy(.accessory)
        if UserDefaults.standard.object(forKey: "pipAlpha") != nil {
            let v = CGFloat(UserDefaults.standard.double(forKey: "pipAlpha"))
            if v >= 0.2 && v <= 1.0 { pipAlpha = v }
        }
        ensureServer()

        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        statusItem.behavior = []          // never auto-remove
        statusItem.isVisible = true
        statusItem.button?.image = statusImage
        statusItem.button?.imagePosition = .imageOnly
        statusItem.button?.title = ""
        statusItem.button?.toolTip = "Claude Wall"
        // any menubar click → custom popover
        statusItem.button?.target = self
        statusItem.button?.action = #selector(statusClicked)
        statusItem.button?.sendAction(on: [.leftMouseUp, .rightMouseUp])

        // custom popover (counts + transparency slider + actions)
        popover.behavior = .transient
        popover.animates = true
        popover.contentViewController = statusPanel
        popover.contentSize = NSSize(width: 320, height: 552)
        statusPanel.onOpen = { [weak self] in self?.popover.performClose(nil); self?.showPicker() }
        statusPanel.onOpenActive = { [weak self] in self?.popover.performClose(nil); self?.openActive() }
        statusPanel.onOpenDashboard = { [weak self] in
            self?.popover.performClose(nil)
            if let u = URL(string: WALL) { NSWorkspace.shared.open(u) }
        }
        statusPanel.onRestart = { [weak self] in self?.popover.performClose(nil); self?.restartServer() }
        statusPanel.onToggleButton = { [weak self] in self?.toggleButton() }
        statusPanel.onCloseAll = { [weak self] in self?.closeAll() }
        statusPanel.onQuit = { [weak self] in self?.quit() }
        statusPanel.onAlpha = { [weak self] v in self?.setPipAlpha(CGFloat(v)) }

        picker.onOpenAll = { [weak self] items in
            guard let self = self else { return }
            for it in items { self.openPip(target: it.target, title: it.name, tile: false) }
            self.tileOpenPips()
        }

        buttonWindow = ButtonWindow()
        buttonWindow.buttonView.onClick = { [weak self] in self?.showPicker() }
        // right-click the floating button → same custom popover (Quit/slider reachable here)
        buttonWindow.buttonView.onRightClick = { [weak self] in
            guard let self = self, let v = self.buttonWindow.contentView else { return }
            self.showPopover(from: v, edge: .minY)
        }
        buttonWindow.orderFrontRegardless()

        pollSummary()
        pollTimer = Timer.scheduledTimer(withTimeInterval: 2.5, repeats: true) { [weak self] _ in
            self?.pollSummary()
        }
        registerHotkey()
    }

    // global ⌘⌥P → open picker (Carbon hotkey; no Accessibility permission needed)
    func registerHotkey() {
        var spec = EventTypeSpec(eventClass: OSType(kEventClassKeyboard), eventKind: OSType(kEventHotKeyPressed))
        InstallEventHandler(GetApplicationEventTarget(), { (_, _, _) -> OSStatus in
            DispatchQueue.main.async { (NSApp.delegate as? AppDelegate)?.showPicker() }
            return noErr
        }, 1, &spec, nil, nil)
        let hkID = EventHotKeyID(signature: OSType(0x50495031), id: 1) // 'PIP1'
        RegisterEventHotKey(UInt32(kVK_ANSI_P), UInt32(cmdKey | optionKey), hkID,
                            GetApplicationEventTarget(), 0, &hotKeyRef)
    }

    // ── bundled claude-wall server: reuse if already running, else spawn+own ──
    func ensureServer() {
        probeHealth { ok in
            if ok { return }                 // something is already serving on 7685
            DispatchQueue.main.async { self.spawnServer() }
        }
    }

    func probeHealth(_ cb: @escaping (Bool) -> Void) {
        guard let u = URL(string: "\(WALL)/api/health") else { cb(false); return }
        var req = URLRequest(url: u); req.timeoutInterval = 1.2
        URLSession.shared.dataTask(with: req) { _, resp, _ in
            cb((resp as? HTTPURLResponse)?.statusCode == 200)
        }.resume()
    }

    func spawnServer() {
        guard let res = Bundle.main.resourcePath else { return }
        let bin = res + "/claude-wall"
        guard FileManager.default.isExecutableFile(atPath: bin) else {
            NSLog("Claude Wall: bundled server missing at \(bin)"); return
        }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: bin)
        p.arguments = ["--serve", "--port", "7685"]
        var env = ProcessInfo.processInfo.environment
        // Finder-launched apps get a minimal PATH; the server needs tmux/git.
        env["PATH"] = (env["PATH"].map { $0 + ":" } ?? "") + "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
        p.environment = env
        p.terminationHandler = { [weak self] _ in
            // respawn if our owned server dies while we're still running
            guard let self = self, self.serverProcess != nil else { return }
            DispatchQueue.main.asyncAfter(deadline: .now() + 1) { self.spawnServer() }
        }
        do { try p.run(); serverProcess = p } catch { NSLog("Claude Wall: server spawn failed \(error)") }
    }

    func applicationWillTerminate(_ note: Notification) {
        if let p = serverProcess { serverProcess = nil; p.terminate() } // only kill the one we spawned
    }

    // Restart the claude-wall server and take ownership of the fresh instance.
    func restartServer() {
        if let p = serverProcess {
            serverProcess = nil          // stop terminationHandler from auto-respawning
            p.terminate()
        } else {
            // reused an external server (e.g. tmux) — free the port so we can own it
            let kill = Process()
            kill.executableURL = URL(fileURLWithPath: "/usr/bin/pkill")
            kill.arguments = ["-f", "claude-wall --serve"]
            try? kill.run()
        }
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.7) { [weak self] in self?.spawnServer() }
    }

    // ── finance stats for the popover ────────────────────────────────────────
    func todayString() -> String {
        let df = DateFormatter(); df.dateFormat = "yyyy-MM-dd"; df.locale = Locale(identifier: "en_US_POSIX")
        return df.string(from: Date())
    }

    func renderUsage() {
        if let u = lastUsage {
            statusPanel.setUsage(u, lastLimits)
            statusPanel.setRepoUsage(u, lastLimits)
        }
    }

    func fetchStats() {
        if let u = URL(string: "\(WALL)/api/usage") {
            URLSession.shared.dataTask(with: u) { data, _, _ in
                guard let usage = data.flatMap({ try? JSONDecoder().decode(Usage.self, from: $0) }) else { return }
                DispatchQueue.main.async { self.lastUsage = usage; self.renderUsage() }
            }.resume()
        }
        // real subscription usage (from Claude's /usage). Empty until keychain access is allowed.
        if let u = URL(string: "\(WALL)/api/limits") {
            URLSession.shared.dataTask(with: u) { data, _, _ in
                let lim = data.flatMap { try? JSONDecoder().decode(Limits.self, from: $0) }
                DispatchQueue.main.async { self.lastLimits = lim; self.renderUsage() }
            }.resume()
        }
    }

    @objc func statusClicked() {
        guard let b = statusItem.button else { return }
        showPopover(from: b, edge: .maxY)
    }

    func showPopover(from view: NSView, edge: NSRectEdge) {
        if popover.isShown { popover.performClose(nil); return }
        statusPanel.setAlpha(Double(pipAlpha))
        refreshPopoverCounts()
        fetchStats()
        NSApp.activate(ignoringOtherApps: true)
        popover.show(relativeTo: view.bounds, of: view, preferredEdge: edge)
    }

    func refreshPopoverCounts() {
        var idle = 0, working = 0, pending = 0, stale = 0
        for (_, s) in statusByTarget {
            switch s {
            case "working": working += 1
            case "permission": pending += 1
            case "stale": stale += 1
            default: idle += 1
            }
        }
        statusPanel.setCounts(working: working, pending: pending, stale: stale, idle: idle, total: statusByTarget.count)
    }

    func setPipAlpha(_ v: CGFloat) {
        pipAlpha = v
        UserDefaults.standard.set(Double(v), forKey: "pipAlpha")
        for (_, bg) in pipBgs { bg.alphaValue = v }
    }

    // ── status polling ───────────────────────────────────────────────────────
    func pollSummary() {
        guard let url = URL(string: "\(WALL)/api/summary") else { return }
        URLSession.shared.dataTask(with: url) { data, _, _ in
            let s = data.flatMap { try? JSONDecoder().decode(Summary.self, from: $0) }
            DispatchQueue.main.async { self.applyStatus(s) }
        }.resume()
    }

    func applyStatus(_ s: Summary?) {
        guard let s = s else {
            statusItem.button?.title = ""
            return
        }
        var idle = 0, working = 0, pending = 0, stale = 0
        var map: [String: String] = [:]
        for p in s.panes {
            map[p.target] = p.status
            switch p.status {
            case "working":    working += 1
            case "permission": pending += 1
            case "stale":      stale += 1
            default:           idle += 1
            }
        }
        statusByTarget = map

        // menubar: icon only; counts live in the tooltip + floating button badges
        statusItem.button?.toolTip = "\(s.total) panes · \(working) working · \(pending) permission · \(stale) idle 30m+ · \(idle) idle"

        buttonWindow.buttonView.setStatus(idle: idle, working: working, pending: pending, stale: stale)
        picker.refreshStatuses(map)
        if popover.isShown {
            statusPanel.setCounts(working: working, pending: pending, stale: stale, idle: idle, total: s.panes.count)
            fetchStats()
        }
    }

    @objc func toggleButton() {
        if buttonWindow.isVisible { buttonWindow.orderOut(nil) }
        else { buttonWindow.orderFrontRegardless() }
    }

    @objc func quit() { NSApp.terminate(nil) }

    // Open every working / waiting-for-answer (permission or idle-30m) pane at once,
    // then tile them side-by-side (no overlap), like the claude-wall web grid.
    @objc func openActive() {
        guard let url = URL(string: "\(WALL)/api/panes") else { return }
        URLSession.shared.dataTask(with: url) { data, _, _ in
            let panes = (data.flatMap { try? JSONDecoder().decode([Pane].self, from: $0) }) ?? []
            DispatchQueue.main.async {
                let wanted: Set<String> = ["working", "permission", "stale"]
                let active = panes.filter { wanted.contains(self.statusByTarget[$0.target] ?? "idle") }
                for p in active {
                    self.openPip(target: p.target, title: p.dirName.isEmpty ? p.session : p.dirName, tile: false)
                }
                self.tileOpenPips()
            }
        }.resume()
    }

    // Lay out all open PiP windows in a grid on the main screen.
    func tileOpenPips() {
        guard let scr = NSScreen.main else { return }
        let f = scr.visibleFrame
        let wins = Array(pips.values)
        let n = wins.count
        guard n > 0 else { return }
        let cols = Int(ceil(Double(n).squareRoot()))
        let rows = Int(ceil(Double(n) / Double(cols)))
        let gap: CGFloat = 8
        let cellW = (f.width - gap * CGFloat(cols + 1)) / CGFloat(cols)
        let cellH = (f.height - gap * CGFloat(rows + 1)) / CGFloat(rows)
        for (i, w) in wins.enumerated() {
            let c = i % cols, rIdx = i / cols
            let x = f.minX + gap + CGFloat(c) * (cellW + gap)
            let y = f.maxY - gap - CGFloat(rIdx + 1) * cellH - CGFloat(rIdx) * gap
            w.setFrame(NSRect(x: x, y: y, width: cellW, height: cellH), display: true)
        }
    }

    @objc func showPicker() {
        guard let url = URL(string: "\(WALL)/api/panes") else { return }
        URLSession.shared.dataTask(with: url) { data, _, _ in
            let panes = (data.flatMap { try? JSONDecoder().decode([Pane].self, from: $0) }) ?? []
            DispatchQueue.main.async {
                let items = panes.map { ($0, self.statusByTarget[$0.target] ?? "idle") }
                self.picker.show(items) { target, name in self.openPip(target: target, title: name) }
            }
        }.resume()
    }

    func openPip(target: String, title: String, tile: Bool = true) {
        if let w = pips[target] { w.makeKeyAndOrderFront(nil); NSApp.activate(ignoringOtherApps: true); return }

        let size = NSRect(x: 0, y: 0, width: 560, height: 420)
        let win = NSWindow(contentRect: size,
                           styleMask: [.titled, .closable, .resizable, .miniaturizable],
                           backing: .buffered, defer: false)
        win.title = "PiP · \(title)"
        win.level = .floating
        win.collectionBehavior = [.canJoinAllSpaces, .stationary, .fullScreenAuxiliary]
        win.isReleasedWhenClosed = false
        win.delegate = self
        win.tabbingMode = .disallowed
        win.isOpaque = false
        win.backgroundColor = .clear

        let container = NSView(frame: size)
        container.autoresizingMask = [.width, .height]
        win.contentView = container

        // native dark backdrop — its alpha IS the transparency (slider-controlled).
        // Sits behind a fully-transparent webview, so terminal text stays opaque.
        let bgView = NSView(frame: size)
        bgView.autoresizingMask = [.width, .height]
        bgView.wantsLayer = true
        bgView.layer?.backgroundColor = NSColor(calibratedRed: 0.055, green: 0.065, blue: 0.10, alpha: 1).cgColor
        bgView.alphaValue = pipAlpha
        container.addSubview(bgView)
        pipBgs[target] = bgView

        let wv = WKWebView(frame: size, configuration: WKWebViewConfiguration())
        wv.autoresizingMask = [.width, .height]
        wv.setValue(false, forKey: "drawsBackground")   // transparent webview → shows the dark layer behind
        if #available(macOS 12.0, *) { wv.underPageBackgroundColor = .clear }
        wv.wantsLayer = true
        wv.layer?.backgroundColor = NSColor.clear.cgColor
        wv.navigationDelegate = self
        container.addSubview(wv)   // above bgView

        // loading spinner until the page finishes loading
        let spin = NSProgressIndicator(frame: NSRect(x: size.width / 2 - 16, y: size.height / 2 - 16, width: 32, height: 32))
        spin.style = .spinning
        spin.isIndeterminate = true
        spin.autoresizingMask = [.minXMargin, .maxXMargin, .minYMargin, .maxYMargin]
        spin.startAnimation(nil)
        container.addSubview(spin)
        spinners[ObjectIdentifier(wv)] = spin

        let enc = target.addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed) ?? target
        if let u = URL(string: "\(WALL)/pip.html?target=\(enc)") { wv.load(URLRequest(url: u)) }

        if tile, let scr = NSScreen.main {
            let f = scr.visibleFrame
            let off = CGFloat(pips.count) * 28
            win.setFrameOrigin(NSPoint(x: f.maxX - 560 - 24 - off, y: f.maxY - 440 - off))
        }
        pips[target] = win
        win.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    @objc func closeAll() {
        for (_, w) in pips { w.close() }
        pips.removeAll()
        pipBgs.removeAll()
    }

    // WKNavigationDelegate — hide the spinner once loaded (or failed)
    private func stopSpinner(_ w: WKWebView) {
        let id = ObjectIdentifier(w)
        spinners[id]?.stopAnimation(nil)
        spinners[id]?.removeFromSuperview()
        spinners[id] = nil
    }
    func webView(_ w: WKWebView, didFinish n: WKNavigation!) { stopSpinner(w) }
    func webView(_ w: WKWebView, didFail n: WKNavigation!, withError e: Error) { stopSpinner(w) }
    func webView(_ w: WKWebView, didFailProvisionalNavigation n: WKNavigation!, withError e: Error) { stopSpinner(w) }

    func windowWillClose(_ notification: Notification) {
        guard let w = notification.object as? NSWindow else { return }
        let gone = pips.filter { $0.value === w }.map { $0.key }
        pips = pips.filter { $0.value !== w }
        for t in gone { pipBgs[t] = nil }
    }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.run()
