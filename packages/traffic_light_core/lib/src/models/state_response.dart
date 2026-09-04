import 'agent_state.dart';

/// One tool's aggregated status, as returned by `GET /state`.
///
/// Mirrors `ToolStatus` in the server's `internal/types`. Data only — see
/// PRD §11.
class ToolStatus {
  const ToolStatus({
    required this.tool,
    required this.state,
    required this.since,
    required this.activeSessions,
    required this.waitingTooLong,
  });

  /// Vendor name as the server reports it ("claude", "copilot", ...).
  /// Not an enum: protocol.md §4 says the source set is open, and a
  /// client that met an unknown vendor should still display it.
  final String tool;

  final AgentState state;

  /// When this tool entered [state]. Null if the server sent something
  /// unparseable — a bad timestamp should not cost us the whole row.
  final DateTime? since;

  final int activeSessions;

  /// Server's judgement that a WAITING state has lasted long enough to
  /// deserve extra emphasis. A display hint, not a distinct state
  /// (protocol.md §5) — the state stays WAITING.
  final bool waitingTooLong;

  factory ToolStatus.fromJson(String tool, Map<String, dynamic> json) {
    return ToolStatus(
      tool: tool,
      state: AgentState.fromWire(json['state'] as String?),
      since: DateTime.tryParse(json['since'] as String? ?? ''),
      activeSessions: (json['activeSessions'] as num?)?.toInt() ?? 0,
      waitingTooLong: json['waitingTooLong'] as bool? ?? false,
    );
  }

  /// How long this tool has been in its current state, if known.
  Duration? ageAt(DateTime now) =>
      since == null ? null : now.difference(since!);
}

/// The whole `GET /state` payload.
class StateSnapshot {
  const StateSnapshot({
    required this.version,
    required this.updatedAt,
    required this.overallState,
    required this.overallTool,
    required this.tools,
  });

  final int version;
  final DateTime? updatedAt;

  /// The headline state. Taken verbatim from the server's `overall`
  /// field and never recomputed from [tools] — PRD §5 is explicit that
  /// every client must show the same headline, which only holds if they
  /// all read it from one place.
  final AgentState overallState;

  /// Which tool caused the headline state. Empty when nothing is
  /// running, which lets a client say "🔴 Claude needs you" without
  /// working out the "Claude" part for itself.
  final String overallTool;

  /// Per-tool rows, most urgent first.
  final List<ToolStatus> tools;

  factory StateSnapshot.fromJson(Map<String, dynamic> json) {
    final overall = json['overall'] as Map<String, dynamic>? ?? const {};
    final rawTools = json['tools'] as Map<String, dynamic>? ?? const {};

    final tools = <ToolStatus>[
      for (final entry in rawTools.entries)
        if (entry.value is Map<String, dynamic>)
          ToolStatus.fromJson(entry.key, entry.value as Map<String, dynamic>),
    ]..sort((a, b) {
        final byUrgency = a.state.urgency.compareTo(b.state.urgency);
        // Stable, predictable ordering: a list that reshuffles between
        // polls is hard to read at a glance.
        return byUrgency != 0 ? byUrgency : a.tool.compareTo(b.tool);
      });

    return StateSnapshot(
      version: (json['version'] as num?)?.toInt() ?? 0,
      updatedAt: DateTime.tryParse(json['updatedAt'] as String? ?? ''),
      overallState: AgentState.fromWire(overall['state'] as String?),
      overallTool: overall['tool'] as String? ?? '',
      tools: tools,
    );
  }

  /// Whether the tool driving the headline has been waiting long enough
  /// that the server flagged it.
  ///
  /// `overall` carries only a state and a tool name, so this looks up the
  /// matching row. That is presentation convenience, not state
  /// derivation: the value still comes from the server, and no client
  /// decides for itself how long is too long.
  bool get overallWaitingTooLong {
    for (final tool in tools) {
      if (tool.tool == overallTool) return tool.waitingTooLong;
    }
    return false;
  }

  /// A one-line summary for a tray title or notification.
  ///
  /// Always includes the label text, never colour alone (PRD §10).
  String get headline => overallTool.isEmpty
      ? overallState.label
      : '$overallTool ${overallState.label}';

  bool get isIdle => overallState == AgentState.idle && tools.isEmpty;
}
