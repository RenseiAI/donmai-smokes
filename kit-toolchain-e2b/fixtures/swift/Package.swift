// swift-tools-version: 6.0

import PackageDescription

let package = Package(
    name: "SwiftKitFixture",
    products: [
        .library(name: "SwiftKitFixture", targets: ["SwiftKitFixture"]),
    ],
    targets: [
        .target(name: "SwiftKitFixture"),
        .testTarget(
            name: "SwiftKitFixtureTests",
            dependencies: ["SwiftKitFixture"]
        ),
    ]
)
