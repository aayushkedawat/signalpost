/// Shared models and `GET /state` client for the Traffic Light clients.
///
/// Deliberately thin. All state-machine, aggregation and staleness logic
/// lives server-side (PRD sections 4 and 11) — this package exists so the
/// desktop and terminal clients agree on data shapes, not so they can
/// reason about state themselves.
library;

export 'src/api/state_client.dart';
export 'src/models/agent_state.dart';
export 'src/models/state_response.dart';
