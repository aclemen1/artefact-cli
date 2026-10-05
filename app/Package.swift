// swift-tools-version:6.0
import PackageDescription

let package = Package(
    name: "Artefact",
    platforms: [.macOS(.v14)],
    targets: [
        .executableTarget(name: "Artefact", path: "Sources/Artefact"),
    ]
)
