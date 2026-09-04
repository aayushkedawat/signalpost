import 'dart:convert';
import 'dart:io';

import 'package:http/http.dart' as http;
import 'package:http/testing.dart';
import 'package:test/test.dart';
import 'package:traffic_light_core/traffic_light_core.dart';

/// A real `GET /state` body, copied from the running server rather than
/// invented, so the models are checked against the shape they will
/// actually meet.
const realBody = '''
{
  "version": 1,
  "updatedAt": "2026-09-04T03:01:47Z",
  "overall": {"state": "waiting", "tool": "claude"},
  "tools": {
    "claude":  {"state": "waiting",   "since": "2026-09-04T03:01:47Z", "activeSessions": 2, "waitingTooLong": true},
    "copilot": {"state": "executing", "since": "2026-09-04T03:00:00Z", "activeSessions": 1, "waitingTooLong": false}
  }
}
''';

StateClient clientReturning(
  int status,
  String body, {
  void Function(http.Request)? onRequest,
}) {
  return StateClient(
    token: 'test-token',
    httpClient: MockClient((req) async {
      onRequest?.call(req);
      return http.Response(body, status);
    }),
  );
}

void main() {
  group('AgentState', () {
    test('parses every wire value the server can send', () {
      const wire = {
        'waiting': AgentState.waiting,
        'error': AgentState.error,
        'unknown': AgentState.unknown,
        'executing': AgentState.executing,
        'done': AgentState.done,
        'idle': AgentState.idle,
      };
      for (final entry in wire.entries) {
        expect(AgentState.fromWire(entry.key), entry.value);
      }
    });

    test('degrades to unknown rather than throwing on a future state', () {
      // A client meeting a newer server must not crash or, worse, show a
      // confidently wrong colour.
      expect(AgentState.fromWire('teleporting'), AgentState.unknown);
      expect(AgentState.fromWire(null), AgentState.unknown);
      expect(AgentState.fromWire(''), AgentState.unknown);
    });

    test('urgency matches the protocol ordering in section 9', () {
      final ordered = AgentState.values.toList()
        ..sort((a, b) => a.urgency.compareTo(b.urgency));
      expect(
        ordered.map((s) => s.wireName),
        ['waiting', 'error', 'unknown', 'executing', 'done', 'idle'],
      );
    });

    test('every state has display text, since colour alone is not enough', () {
      // PRD section 10: all surfaces must convey state as text too.
      for (final state in AgentState.values) {
        expect(state.label, isNotEmpty, reason: '${state.name} has no label');
      }
    });
  });

  group('StateSnapshot', () {
    test('parses a real server body', () {
      final snap = StateSnapshot.fromJson(
          jsonDecode(realBody) as Map<String, dynamic>);
      expect(snap.version, 1);
      expect(snap.overallState, AgentState.waiting);
      expect(snap.overallTool, 'claude');
      expect(snap.tools, hasLength(2));
      expect(snap.headline, 'claude needs you');
    });

    test('orders tools by urgency so the list reads top-down', () {
      final snap = StateSnapshot.fromJson(
          jsonDecode(realBody) as Map<String, dynamic>);
      expect(snap.tools.first.tool, 'claude'); // waiting outranks executing
      expect(snap.tools.last.tool, 'copilot');
    });

    test('takes the headline from overall, never recomputing it', () {
      // Deliberately inconsistent: overall says idle while a tool says
      // waiting. The server is the authority (PRD section 4), so a client
      // that "helpfully" recomputed would disagree with every other
      // client — the one thing the overall field exists to prevent.
      final snap = StateSnapshot.fromJson({
        'version': 1,
        'overall': {'state': 'idle', 'tool': ''},
        'tools': {
          'claude': {'state': 'waiting', 'activeSessions': 1},
        },
      });
      expect(snap.overallState, AgentState.idle);
      expect(snap.tools.single.state, AgentState.waiting);
    });

    test('survives a body with missing and malformed fields', () {
      final snap = StateSnapshot.fromJson({
        'tools': {
          'claude': {'state': 'waiting', 'since': 'not-a-date'},
          'ghost': 'not-an-object',
        },
      });
      expect(snap.version, 0);
      expect(snap.updatedAt, isNull);
      expect(snap.tools, hasLength(1), reason: 'the junk row is dropped');
      expect(snap.tools.single.since, isNull);
      expect(snap.tools.single.activeSessions, 0);
    });

    test('handles an empty state, which is the common case', () {
      final snap = StateSnapshot.fromJson({
        'version': 1,
        'overall': {'state': 'idle', 'tool': ''},
        'tools': <String, dynamic>{},
      });
      expect(snap.isIdle, isTrue);
      expect(snap.headline, 'nothing running');
    });
  });

  group('StateClient', () {
    test('sends the bearer token', () async {
      String? auth;
      final client = clientReturning(200, realBody,
          onRequest: (req) => auth = req.headers['Authorization']);
      await client.fetch();
      expect(auth, 'Bearer test-token');
    });

    test('requests /state', () async {
      String? path;
      final client = clientReturning(200, realBody,
          onRequest: (req) => path = req.url.path);
      await client.fetch();
      expect(path, '/state');
    });

    test('reports unreachable rather than throwing when nothing answers',
        () async {
      final client = StateClient(
        token: 't',
        httpClient: MockClient((_) async => throw const SocketException('refused')),
      );
      final result = await client.fetch();
      expect(result.ok, isFalse);
      expect(result.failure, PollFailure.unreachable);
      expect(result.detail, contains('Is the server running?'));
    });

    test('distinguishes a rejected token from an unreachable server',
        () async {
      // These need different remedies — start the server, versus your
      // token is stale — so the UI must be able to tell them apart.
      final client = clientReturning(401, 'unauthorized');
      final result = await client.fetch();
      expect(result.failure, PollFailure.unauthorized);
    });

    test('reports a bad response rather than pretending it is idle',
        () async {
      final client = clientReturning(200, 'this is not json');
      final result = await client.fetch();
      expect(result.failure, PollFailure.badResponse);
    });

    test('treats a non-object body as unusable', () async {
      final client = clientReturning(200, '[1,2,3]');
      expect((await client.fetch()).failure, PollFailure.badResponse);
    });

    test('reports a 500 without crashing', () async {
      final client = clientReturning(500, 'boom');
      expect((await client.fetch()).failure, PollFailure.badResponse);
    });

    test('a missing token is reported, not thrown', () async {
      final client = StateClient(
        token: '',
        httpClient: MockClient((_) async => http.Response(realBody, 200)),
      );
      final result = await client.fetch();
      expect(result.failure, PollFailure.unauthorized);
      expect(result.detail, contains('Start the server'));
    });
  });
}
