import AppKit

// The Finder icons and Applications link remain interactive. Only the short
// installation guidance and arrow are rendered into the background.
guard CommandLine.arguments.count == 2 else {
    fatalError("usage: render-dmg-background.swift OUTPUT_PNG")
}
let size = NSSize(width: 720, height: 420)
let image = NSImage(size: size)
image.lockFocus()
NSColor(calibratedWhite: 0.97, alpha: 1).setFill()
NSRect(origin: .zero, size: size).fill()

func drawText(_ text: String, y: CGFloat, size: CGFloat, weight: NSFont.Weight, shade: CGFloat) {
    let paragraph = NSMutableParagraphStyle()
    paragraph.alignment = .center
    (text as NSString).draw(in: NSRect(x: 30, y: y, width: 660, height: 48), withAttributes: [
        .font: NSFont.systemFont(ofSize: size, weight: weight),
        .foregroundColor: NSColor(calibratedWhite: shade, alpha: 1),
        .paragraphStyle: paragraph,
    ])
}

drawText("Cineko", y: 340, size: 32, weight: .semibold, shade: 0.12)
drawText("Drag Cineko Launcher to Applications", y: 293, size: 17, weight: .regular, shade: 0.38)
drawText("If Cineko is already installed, choose Replace.", y: 44, size: 13, weight: .regular, shade: 0.45)

NSColor(calibratedWhite: 0.65, alpha: 1).setStroke()
let arrow = NSBezierPath()
arrow.lineWidth = 2.5
arrow.lineCapStyle = .round
arrow.lineJoinStyle = .round
arrow.move(to: NSPoint(x: 336, y: 205))
arrow.line(to: NSPoint(x: 384, y: 205))
arrow.move(to: NSPoint(x: 373, y: 216))
arrow.line(to: NSPoint(x: 384, y: 205))
arrow.line(to: NSPoint(x: 373, y: 194))
arrow.stroke()
image.unlockFocus()

guard let tiff = image.tiffRepresentation,
      let bitmap = NSBitmapImageRep(data: tiff),
      let png = bitmap.representation(using: .png, properties: [:]) else {
    fatalError("could not render the installer background")
}
try png.write(to: URL(fileURLWithPath: CommandLine.arguments[1]))
