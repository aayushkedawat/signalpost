import Cocoa
import FlutterMacOS

@main
class AppDelegate: FlutterAppDelegate {
  // This app has no window: it lives in the menu bar (LSUIElement in
  // Info.plist) and hides its window at startup. The Flutter template
  // returns true here, which quits the app the moment that window is
  // hidden — the app launched and immediately exited until this was
  // changed. A menu bar app must outlive its windows.
  override func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
    return false
  }

  override func applicationSupportsSecureRestorableState(_ app: NSApplication) -> Bool {
    return true
  }
}
