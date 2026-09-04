import 'dart:async';
import 'dart:io';

import 'package:traffic_light_core/traffic_light_core.dart';
import 'package:tray_manager/tray_manager.dart';

/// Diagnostics are opt-in, matching the hook binary's
/// TRAFFIC_LIGHT_HOOK_DEBUG convention. A menu bar app has nowhere to
/// show errors, so when it misbehaves this is the only way in.
final bool _debug =
    Platform.environment['TRAFFIC_LIGHT_DESKTOP_DEBUG']?.isNotEmpty ?? false;

void _log(String message) {
  if (_debug) stderr.writeln('traffic-light-desktop: $message');
}

/// How the six states are shown in the menu bar.
///
/// Colours match `apps/cli` exactly (waiting red, error magenta, unknown
/// cyan, executing yellow, done green, idle grey) so the two surfaces
/// never disagree about what a colour means. The glyph is decoration:
/// the label beside it is what carries the meaning, because PRD §10
/// requires every surface to convey state as text and not colour alone.
const _glyphs = <AgentState, String>{
  AgentState.waiting: '🔴',
  AgentState.error: '🟣',
  AgentState.unknown: '🔵',
  AgentState.executing: '🟡',
  AgentState.done: '🟢',
  AgentState.idle: '⚪',
};

/// Deliberately not a circle. "I cannot reach the server" is a fact about
/// this client, not a state any agent is in, and dressing it as another
/// coloured dot would be the app inventing a state the server never
/// reported. A user seeing this needs to start the server, not go and
/// look at an agent.
const _offlineGlyph = '⚠︎';

/// Owns the menu bar item and the poll loop.
class TrayController with TrayListener {
  TrayController(
    this._client, {
    Duration? interval,
  }) : interval = interval ?? _defaultInterval();

  /// 300ms rather than a second.
  ///
  /// The dominant delay before the light turns red is Claude Code's own
  /// 6.0s timer before it raises the permission notification (measured
  /// across 67 samples at 5.99–6.02s), which nothing here can shorten:
  /// a shorter timer of our own cannot tell a slow auto-approved command
  /// from a human being asked, and reintroduces false reds. Polling is
  /// the only part of the delay we own, so it should not be the part
  /// anyone notices. On loopback each poll is a sub-millisecond request
  /// against an in-memory map.
  static Duration _defaultInterval() {
    final override = Platform.environment['TRAFFIC_LIGHT_POLL_MS'];
    final ms = int.tryParse(override ?? '');
    return Duration(milliseconds: ms != null && ms > 0 ? ms : 300);
  }

  final StateClient _client;
  final Duration interval;

  Timer? _timer;
  String? _lastTitle;
  PollResult? _last;

  Future<void> start() async {
    trayManager.addListener(this);

    // setIcon is the *only* place tray_manager constructs its
    // NSStatusItem (TrayManagerPlugin.swift: `if (trayIcon == nil) {
    // trayIcon = TrayIcon() }`). Every other call, setTitle included,
    // goes through `trayIcon?.` — so without this the app runs, polls
    // happily, reports no error, and puts nothing in the menu bar.
    //
    // The image is deliberately transparent: the visible content is the
    // title, which carries the glyph and the label together. Colouring
    // the icon instead would mean six PNGs per appearance and would put
    // the meaning back into colour alone, which PRD §10 forbids.
    await trayManager.setIcon('assets/tray_placeholder.png');
    await trayManager.setTitle('$_offlineGlyph starting…');
    await _refresh();
    _timer = Timer.periodic(interval, (_) => _refresh());
    _log('polling ${_client.baseUrl} every ${interval.inMilliseconds}ms');

    // Confirms the NSStatusItem actually exists and has been given a
    // position in the menu bar. Null or zero-width here means the item
    // was never created, which is otherwise completely silent.
    if (_debug) {
      final bounds = await trayManager.getBounds();
      _log(bounds == null
          ? 'NO STATUS ITEM — nothing will appear in the menu bar'
          : 'status item at ${bounds.left.toStringAsFixed(0)},'
              '${bounds.top.toStringAsFixed(0)} '
              'size ${bounds.width.toStringAsFixed(0)}x'
              '${bounds.height.toStringAsFixed(0)}');
    }
  }

  Future<void> dispose() async {
    _timer?.cancel();
    trayManager.removeListener(this);
    _client.close();
  }

  Future<void> _refresh() async {
    final result = await _client.fetch();
    if (!result.ok) _log('poll failed: ${result.failure?.name} — ${result.detail}');
    _last = result;

    final title = _titleFor(result);
    // Only touch the menu bar when the text actually changes. Rewriting
    // it every second makes the item flicker and, on some macOS
    // versions, steal focus from the menu while it is open.
    if (title != _lastTitle) {
      _lastTitle = title;
      await trayManager.setTitle(title);
    }
    await trayManager.setContextMenu(_menuFor(result));
  }

  /// The headline shown in the menu bar itself.
  String _titleFor(PollResult result) {
    final snapshot = result.snapshot;
    if (snapshot == null) return '$_offlineGlyph offline';

    final glyph = _glyphs[snapshot.overallState] ?? _offlineGlyph;
    if (snapshot.isIdle) return glyph;

    // "🔴 claude" — the tool name is the useful half when something
    // needs you and more than one agent is running. The full label is
    // one click away in the menu.
    final tool = snapshot.overallTool;
    return tool.isEmpty ? glyph : '$glyph $tool';
  }

  /// The detail popover: one row per tool, plus whatever the user needs
  /// to know when there is nothing to show.
  Menu _menuFor(PollResult result) {
    final items = <MenuItem>[];
    final snapshot = result.snapshot;

    if (snapshot == null) {
      items.add(MenuItem(label: 'Traffic Light — offline', disabled: true));
      items.add(MenuItem.separator());
      // The remedy differs by failure, so say which one happened rather
      // than a generic error.
      items.add(MenuItem(label: result.detail, disabled: true));
    } else if (snapshot.tools.isEmpty) {
      items.add(MenuItem(label: 'No agents running', disabled: true));
    } else {
      for (final tool in snapshot.tools) {
        items.add(MenuItem(
          label: _rowFor(tool),
          disabled: true,
        ));
      }
      items.add(MenuItem.separator());
      final updated = snapshot.updatedAt;
      items.add(MenuItem(
        label: updated == null
            ? 'Server connected'
            : 'Updated ${_clock(updated.toLocal())}',
        disabled: true,
      ));
    }

    items.add(MenuItem.separator());
    items.add(MenuItem(key: 'quit', label: 'Quit Traffic Light'));
    return Menu(items: items);
  }

  String _rowFor(ToolStatus tool) {
    final glyph = _glyphs[tool.state] ?? _offlineGlyph;
    final buffer = StringBuffer('$glyph  ${tool.tool} — ${tool.state.label}');

    // waitingTooLong is emphasis, not a distinct state (protocol.md §5),
    // so it annotates the row rather than changing its colour.
    if (tool.waitingTooLong) buffer.write(' (a while now)');

    if (tool.activeSessions > 1) {
      buffer.write('  ·  ${tool.activeSessions} sessions');
    }
    return buffer.toString();
  }

  String _clock(DateTime t) =>
      '${t.hour.toString().padLeft(2, '0')}:'
      '${t.minute.toString().padLeft(2, '0')}:'
      '${t.second.toString().padLeft(2, '0')}';

  /// Left-click opens the same menu as right-click, which is what a menu
  /// bar item is expected to do on macOS.
  @override
  void onTrayIconMouseDown() => trayManager.popUpContextMenu();

  @override
  void onTrayIconRightMouseDown() => trayManager.popUpContextMenu();

  @override
  void onTrayMenuItemClick(MenuItem menuItem) {
    if (menuItem.key == 'quit') {
      dispose();
      exit(0);
    }
  }

  /// Exposed for tests.
  PollResult? get lastResult => _last;
  String? get lastTitle => _lastTitle;
  String titleForTesting(PollResult r) => _titleFor(r);
  String rowForTesting(ToolStatus t) => _rowFor(t);
}
