// Swift Testing (bundled with the Swift 6 toolchain; the kit ships a
// swift-testing skill, so the smoke exercises the same framework).
import Testing
@testable import SwiftKitFixture

@Test func addTwoAndThree() {
    #expect(add(2, 3) == 5)
    print("KIT_TOOLCHAIN_SWIFT_TEST_OK")
}
