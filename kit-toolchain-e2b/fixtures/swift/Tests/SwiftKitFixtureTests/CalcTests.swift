import XCTest
@testable import SwiftKitFixture

final class CalcTests: XCTestCase {
    func testAdd() {
        XCTAssertEqual(Calculator.add(2, 3), 5)
        print("KIT_TOOLCHAIN_SWIFT_TEST_OK")
    }
}
