/// The six states a session or tool can be in.
///
/// This mirrors `docs/protocol.md` §2. Clients never compute a state —
/// they only render one the server has already decided (PRD §4, §11).
/// The priority ordering lives here too, but only so a client can sort a
/// list for display; the headline state always comes from the server's
/// own `overall` field rather than being re-derived (PRD §5).
enum AgentState {
  /// Needs a human: a permission prompt or a question is outstanding.
  waiting('waiting', 'needs you'),

  /// Something went wrong and was reported explicitly. Never inferred.
  error('error', 'failed'),

  /// The server has lost confidence in what this session is doing.
  unknown('unknown', 'not sure'),

  /// Working. No attention required.
  executing('executing', 'working'),

  /// Just finished. Ephemeral — the server drops it back to idle.
  done('done', 'finished'),

  /// Alive but not working.
  idle('idle', 'nothing running');

  const AgentState(this.wireName, this.label);

  /// The value as it appears in `GET /state` JSON.
  final String wireName;

  /// Human-readable text shown beside (never instead of) the colour.
  /// PRD §10 requires every surface to convey state as text as well as
  /// colour, for colour-blindness and theme variance.
  final String label;

  /// Parses a wire value, falling back to [AgentState.unknown].
  ///
  /// Deliberately total rather than throwing: a client that cannot
  /// understand a future server's vocabulary should degrade to "not
  /// sure", which is exactly what UNKNOWN means, instead of crashing or
  /// showing a confidently wrong colour.
  static AgentState fromWire(String? value) {
    for (final state in AgentState.values) {
      if (state.wireName == value) return state;
    }
    return AgentState.unknown;
  }

  /// Rank for display ordering, lower is more urgent.
  ///
  /// Matches the aggregation order in protocol.md §9
  /// (WAITING > ERROR > UNKNOWN > EXECUTING > DONE > IDLE). Used only to
  /// sort rows in a list — never to pick the headline state, which the
  /// server supplies.
  int get urgency => index;

  /// Whether this state is asking for a human, and so should be allowed
  /// to interrupt. Used to decide emphasis, not to decide state.
  bool get needsAttention =>
      this == AgentState.waiting || this == AgentState.error;
}
