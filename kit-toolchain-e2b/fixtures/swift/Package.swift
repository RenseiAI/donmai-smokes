// swift-tools-version:6.0
// Minimal PURE-SWIFT SPM fixture for the kit-toolchain e2b proof.
// Deliberately dependency-free (no network beyond the toolchain) and free of
// Apple-only imports (UIKit/SwiftUI do NOT build on Linux — the swift kit's
// Linux lane is logic modules only).
import PackageDescription

let package = Package(
    name: "SwiftKitFixture",
    products: [
        .library(name: "SwiftKitFixture", targets: ["SwiftKitFixture"]),
    ],
    targets: [
        .target(name: "SwiftKitFixture"),
        .testTarget(name: "SwiftKitFixtureTests", dependencies: ["SwiftKitFixture"]),
    ]
)
