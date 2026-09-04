import 'dart:convert';
import 'dart:io';

import 'package:http/http.dart' as http;

import '../models/state_response.dart';

/// Why a poll failed. Kept separate from [AgentState] on purpose: "I
/// cannot reach the server" is a fact about this client's network, not a
/// statement about what any agent is doing.
///
/// Collapsing the two would be the client telling a lie the server never
/// told — showing grey/idle when the truth is that we have no idea. PRD
/// §6 makes reachability explicitly a client-side question.
enum PollFailure {
  /// No server listening, DNS failure, timeout, connection refused.
  unreachable,

  /// Reached the server but the token was missing or rejected.
  unauthorized,

  /// Reached the server, but it answered with something unusable.
  badResponse,
}

/// The outcome of one poll: either a snapshot, or a reason there isn't one.
class PollResult {
  const PollResult.success(this.snapshot)
      : failure = null,
        detail = '';

  const PollResult.failed(this.failure, this.detail) : snapshot = null;

  final StateSnapshot? snapshot;
  final PollFailure? failure;

  /// Human-readable context for [failure]. Shown in the UI so a user can
  /// tell "start the server" apart from "your token is stale".
  final String detail;

  bool get ok => snapshot != null;
}

/// Reads `GET /state`. That is the entire client surface: no writes, no
/// event posting, no state derivation (PRD §11 — clients are dumb by
/// design, and §4 keeps the server the sole authority).
class StateClient {
  StateClient({
    Uri? baseUrl,
    String? token,
    http.Client? httpClient,
    this.timeout = const Duration(seconds: 2),
  })  : baseUrl = baseUrl ?? Uri.parse('http://127.0.0.1:8787'),
        _token = token,
        _http = httpClient ?? http.Client();

  final Uri baseUrl;
  final Duration timeout;
  final http.Client _http;
  String? _token;

  /// Default location the server writes its token to on first run.
  static String defaultTokenPath() {
    final home = Platform.environment['HOME'] ?? '';
    return '$home/.traffic-light/token';
  }

  /// Loads the bearer token, preferring an explicit environment override
  /// so the app can be pointed at a test server without touching the
  /// real one.
  ///
  /// Returns null rather than throwing when the file is absent: that
  /// simply means the server has never run, which is a state the UI
  /// should explain rather than crash on.
  static Future<String?> loadToken({String? path}) async {
    final override = Platform.environment['TRAFFIC_LIGHT_TOKEN'];
    if (override != null && override.isNotEmpty) return override.trim();
    try {
      final file = File(path ?? defaultTokenPath());
      if (!await file.exists()) return null;
      final value = (await file.readAsString()).trim();
      return value.isEmpty ? null : value;
    } on FileSystemException {
      // Unreadable (sandbox, permissions) is indistinguishable from
      // absent as far as the UI is concerned.
      return null;
    }
  }

  /// Polls once. Never throws: a display client that crashes on a
  /// network blip is worse than one that says "can't reach the server".
  Future<PollResult> fetch() async {
    final token = _token ??= await loadToken();
    if (token == null || token.isEmpty) {
      return const PollResult.failed(
        PollFailure.unauthorized,
        'No token found. Start the server once to create it.',
      );
    }

    try {
      final response = await _http.get(
        baseUrl.replace(path: '/state'),
        headers: {'Authorization': 'Bearer $token'},
      ).timeout(timeout);

      if (response.statusCode == 401 || response.statusCode == 403) {
        // Force a re-read next time: the server may have regenerated it.
        _token = null;
        return const PollResult.failed(
          PollFailure.unauthorized,
          'Token rejected. It may have been regenerated.',
        );
      }
      if (response.statusCode != 200) {
        return PollResult.failed(
          PollFailure.badResponse,
          'Server returned ${response.statusCode}.',
        );
      }

      final decoded = jsonDecode(response.body);
      if (decoded is! Map<String, dynamic>) {
        return const PollResult.failed(
          PollFailure.badResponse,
          'Server sent a body that was not an object.',
        );
      }
      return PollResult.success(StateSnapshot.fromJson(decoded));
    } on FormatException catch (e) {
      return PollResult.failed(PollFailure.badResponse, 'Bad JSON: ${e.message}');
    } catch (e) {
      // SocketException, TimeoutException, ClientException and anything
      // else all mean the same thing to a user: the light cannot be
      // trusted right now.
      return PollResult.failed(
        PollFailure.unreachable,
        'Cannot reach ${baseUrl.host}:${baseUrl.port}. Is the server running?',
      );
    }
  }

  void close() => _http.close();
}
