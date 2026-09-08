import AppKit
import Foundation

// Regeneration is CI-only. Local checkouts contain the final application assets.
guard ProcessInfo.processInfo.environment["CI"] == "true" else {
    fatalError("Icon rendering is CI-only; local intermediate files are not allowed.")
}
let root = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
let output = root.appendingPathComponent("build/icon-output")
let source = try String(contentsOf: root.appendingPathComponent("build/appicon.svg"), encoding: .utf8)

func png(_ svg: String, size: Int) throws -> Data {
    guard let image = NSImage(data: Data(svg.utf8)),
          let bitmap = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: size, pixelsHigh: size,
            bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
            colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0),
          let context = NSGraphicsContext(bitmapImageRep: bitmap) else {
        fatalError("Cannot render Cineko SVG")
    }
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = context
    context.imageInterpolation = .high
    image.draw(in: NSRect(x: 0, y: 0, width: size, height: size))
    NSGraphicsContext.restoreGraphicsState()
    guard let data = bitmap.representation(using: .png, properties: [:]) else {
        fatalError("Cannot encode Cineko icon")
    }
    return data
}

func save(_ data: Data, _ relative: String) throws {
    let path = output.appendingPathComponent(relative)
    try FileManager.default.createDirectory(at: path.deletingLastPathComponent(), withIntermediateDirectories: true)
    try data.write(to: path)
}

let appIcon = try png(source, size: 1024)
try save(appIcon, "build/appicon.png")
for alias in ["build/iconfile.png", "build/darwin/iconfile.png"] {
    if FileManager.default.fileExists(atPath: root.appendingPathComponent(alias).path) {
        try save(appIcon, alias)
    }
}
let expression = try NSRegularExpression(pattern: #"<g id="cineko-mark"[^>]*>[\s\S]*?</g>"#)
guard let match = expression.firstMatch(in: source, range: NSRange(source.startIndex..., in: source)),
      let range = Range(match.range, in: source) else { fatalError("Canonical Cineko mark is missing") }
let mark = String(source[range]).replacingOccurrences(of: "#ffffff", with: "#000000")
let template = #"<svg xmlns="http://www.w3.org/2000/svg" width="36" height="36" viewBox="0 0 24 24">"# + mark + "</svg>"
if FileManager.default.fileExists(atPath: root.appendingPathComponent("internal/desktop/activation_darwin.m").path) {
    let templatePNG = try png(template, size: 36)
    let header = """
    // Generated from build/appicon.svg by scripts/render-icons.swift; see LICENSE.icons.
    static const char *cinekoStatusIconBase64 = "\(templatePNG.base64EncodedString())";
    
    """
    try save(Data(header.utf8), "internal/desktop/status_icon_darwin.h")
    try save(templatePNG, "build/status-icon.png")
}
print("Rendered canonical Slate icon and existing application icon aliases.")
