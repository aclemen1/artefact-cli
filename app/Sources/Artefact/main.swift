// Artefact.app opens artefact://<sphere>/<id> links: it asks the artefact
// CLI for a local path (`artefact locate`) and opens it in the default app.
import AppKit
import Foundation

func findCLI() -> String? {
    if let env = ProcessInfo.processInfo.environment["ARTEFACT_BIN"], FileManager.default.isExecutableFile(atPath: env) {
        return env
    }
    let home = FileManager.default.homeDirectoryForCurrentUser.path
    for p in ["\(home)/.local/bin/artefact", "\(home)/go/bin/artefact", "/opt/homebrew/bin/artefact", "/usr/local/bin/artefact"]
    where FileManager.default.isExecutableFile(atPath: p) {
        return p
    }
    return nil
}

struct Located: Decodable {
    struct Result: Decodable { let path: String }
    struct Failure: Decodable { let message: String }
    let ok: Bool
    let result: Result?
    let error: Failure?
}

func locate(_ uri: String) -> Result<String, String> {
    guard let cli = findCLI() else {
        return .failure("The artefact command was not found in ~/.local/bin, ~/go/bin, /opt/homebrew/bin or /usr/local/bin.")
    }
    let p = Process()
    p.executableURL = URL(fileURLWithPath: cli)
    p.arguments = ["locate", uri]
    let out = Pipe()
    p.standardOutput = out
    p.standardError = FileHandle.nullDevice
    do { try p.run() } catch { return .failure("Cannot run \(cli): \(error.localizedDescription)") }
    let data = out.fileHandleForReading.readDataToEndOfFile()
    p.waitUntilExit()
    guard let answer = try? JSONDecoder().decode(Located.self, from: data) else {
        return .failure("Unreadable answer from artefact locate.")
    }
    if answer.ok, let r = answer.result { return .success(r.path) }
    return .failure(answer.error?.message ?? "artefact locate failed.")
}

func alert(_ uri: String, _ message: String) {
    NSApp.activate(ignoringOtherApps: true)
    let a = NSAlert()
    a.messageText = "Cannot open \(uri)"
    a.informativeText = message
    a.alertStyle = .warning
    a.runModal()
}

@MainActor
final class Opener: NSObject, NSApplicationDelegate {
    var handled = false

    func applicationWillFinishLaunching(_ notification: Notification) {
        NSAppleEventManager.shared().setEventHandler(
            self, andSelector: #selector(handleURL(_:reply:)),
            forEventClass: AEEventClass(kInternetEventClass), andEventID: AEEventID(kAEGetURL))
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        for arg in CommandLine.arguments.dropFirst() where arg.hasPrefix("artefact://") {
            open(arg)
        }
        // Launched by a link: the Apple event arrives right after launch.
        DispatchQueue.main.asyncAfter(deadline: .now() + 2) { NSApp.terminate(nil) }
    }

    func application(_ application: NSApplication, open urls: [URL]) {
        for u in urls { open(u.absoluteString) }
    }

    @objc func handleURL(_ event: NSAppleEventDescriptor, reply: NSAppleEventDescriptor) {
        if let s = event.paramDescriptor(forKeyword: keyDirectObject)?.stringValue { open(s) }
    }

    func open(_ uri: String) {
        handled = true
        switch locate(uri) {
        case .success(let path):
            NSWorkspace.shared.open(URL(fileURLWithPath: path))
        case .failure(let message):
            alert(uri, message)
        }
    }
}

extension String: @retroactive Error {}

let app = NSApplication.shared
let delegate = Opener()
app.delegate = delegate
app.setActivationPolicy(.accessory)
app.run()
