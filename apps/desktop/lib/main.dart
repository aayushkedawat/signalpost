/// Traffic Light — macOS menu bar app.
///
/// Lives entirely in the menu bar: no window, no Dock icon. It polls
/// `GET /state` and renders what the server says. It computes nothing —
/// the headline state comes from the server's `overall` field, per
/// PRD §4 ("the server is the sole authority over state") and §11
/// ("clients are dumb by design").
library;

import 'package:flutter/widgets.dart';
import 'package:traffic_light_core/traffic_light_core.dart';
import 'package:window_manager/window_manager.dart';

import 'tray_presenter.dart';

Future<void> main() async {
  WidgetsFlutterBinding.ensureInitialized();

  // The app has no UI of its own. Hiding the window before it can be
  // shown avoids a frame of empty window flashing on launch; LSUIElement
  // in Info.plist keeps it out of the Dock and app switcher.
  await windowManager.ensureInitialized();
  await windowManager.setSkipTaskbar(true);
  await windowManager.hide();

  final controller = TrayController(StateClient());
  await controller.start();

  runApp(const _Headless());
}

/// Flutter needs a root widget even when nothing is drawn: the engine
/// drives the run loop that the tray and polling timer depend on.
class _Headless extends StatelessWidget {
  const _Headless();

  @override
  Widget build(BuildContext context) => const SizedBox.shrink();
}
