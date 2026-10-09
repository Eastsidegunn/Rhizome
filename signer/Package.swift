// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "rhizome-signer",
    platforms: [.macOS(.v14)],
    products: [
        .executable(name: "rhizome-signer", targets: ["rhizome-signer"]),
        .executable(name: "rhizome-signer-selftest", targets: ["rhizome-signer-selftest"]),
    ],
    targets: [
        .target(name: "SignerCore"),
        .executableTarget(name: "rhizome-signer", dependencies: ["SignerCore"]),
        .executableTarget(name: "rhizome-signer-selftest", dependencies: ["SignerCore"]),
    ],
    swiftLanguageVersions: [.v5]
)
