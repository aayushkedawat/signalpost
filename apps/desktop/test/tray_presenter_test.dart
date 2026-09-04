import 'package:flutter_test/flutter_test.dart';
import 'package:traffic_light_core/traffic_light_core.dart';
import 'package:traffic_light_desktop/tray_presenter.dart';

TrayController controller() => TrayController(StateClient(token: 'x'));

StateSnapshot snapshot({
  required String overallState,
  String overallTool = '',
  Map<String, dynamic> tools = const {},
}) {
  return StateSnapshot.fromJson({
    'version': 1,
    'updatedAt': '2026-09-04T03:01:47Z',
    'overall': {'state': overallState, 'tool': overallTool},
    'tools': tools,
  });
}

void main() {
  group('menu bar title', () {
    test('names the tool that needs you', () {
      final title = controller().titleForTesting(PollResult.success(
        snapshot(overallState: 'waiting', overallTool: 'claude', tools: {
          'claude': {'state': 'waiting', 'activeSessions': 1},
        }),
      ));
      expect(title, '🔴 claude');
    });

    test('shows a bare glyph when nothing is running', () {
      final title = controller().titleForTesting(
        PollResult.success(snapshot(overallState: 'idle')),
      );
      // No tool name to show, and no text worth spending menu bar width
      // on when the answer is "nothing is happening".
      expect(title, '⚪');
    });

    test('an unreachable server never looks like idle', () {
      // The worst thing this app could do is render a server it cannot
      // reach as a calm grey light. That is the app inventing a state the
      // server never reported.
      final offline = controller().titleForTesting(
        const PollResult.failed(PollFailure.unreachable, 'nope'),
      );
      final idle = controller().titleForTesting(
        PollResult.success(snapshot(overallState: 'idle')),
      );
      expect(offline, isNot(equals(idle)));
      expect(offline, contains('offline'));
      expect(offline, isNot(contains('⚪')));
    });

    test('every failure kind is visibly offline', () {
      for (final failure in PollFailure.values) {
        final title = controller()
            .titleForTesting(PollResult.failed(failure, 'detail'));
        expect(title, contains('offline'), reason: '$failure');
      }
    });

    test('a stale wait is visually distinct from a fresh one', () {
      // Nothing fires when a prompt is answered, so a WAITING that has
      // gone quiet may already have been dealt with. It must not look
      // identical to one that genuinely needs attention now, or the red
      // light stops meaning anything.
      final fresh = controller().titleForTesting(PollResult.success(
        snapshot(overallState: 'waiting', overallTool: 'claude', tools: {
          'claude': {'state': 'waiting', 'activeSessions': 1, 'waitingTooLong': false},
        }),
      ));
      final stale = controller().titleForTesting(PollResult.success(
        snapshot(overallState: 'waiting', overallTool: 'claude', tools: {
          'claude': {'state': 'waiting', 'activeSessions': 1, 'waitingTooLong': true},
        }),
      ));
      expect(fresh, '🔴 claude');
      expect(stale, '🟠 claude');
      expect(fresh, isNot(equals(stale)));
    });

    test('a stale wait is still WAITING, not a new state', () {
      // Display only: the server stays the sole authority (PRD section 4).
      final snap = snapshot(
        overallState: 'waiting',
        overallTool: 'claude',
        tools: {
          'claude': {'state': 'waiting', 'activeSessions': 1, 'waitingTooLong': true},
        },
      );
      expect(snap.overallState, AgentState.waiting);
      expect(snap.tools.single.state, AgentState.waiting);
      expect(snap.overallWaitingTooLong, isTrue);
    });

    test('only WAITING gets the stale treatment', () {
      // waitingTooLong is meaningless on other states; a stray true must
      // not repaint them.
      for (final state in AgentState.values) {
        if (state == AgentState.waiting) continue;
        final title = controller().titleForTesting(PollResult.success(
          snapshot(overallState: state.wireName, overallTool: 't', tools: {
            't': {'state': state.wireName, 'activeSessions': 1, 'waitingTooLong': true},
          }),
        ));
        expect(title, isNot(contains('🟠')), reason: state.name);
      }
    });

    test('each state gets a distinct glyph', () {
      final seen = <String>{};
      for (final state in AgentState.values) {
        final title = controller().titleForTesting(PollResult.success(
          snapshot(overallState: state.wireName, overallTool: 't'),
        ));
        final glyph = title.split(' ').first;
        expect(seen.add(glyph), isTrue,
            reason: '${state.name} reuses glyph $glyph');
      }
    });
  });

  group('tool rows', () {
    ToolStatus tool(Map<String, dynamic> json) =>
        ToolStatus.fromJson('claude', json);

    test('always carries the label, not colour alone', () {
      // PRD section 10: colour-blindness and theme variance mean the text
      // has to stand on its own.
      for (final state in AgentState.values) {
        final row = controller().rowForTesting(
          tool({'state': state.wireName, 'activeSessions': 1}),
        );
        expect(row, contains(state.label), reason: state.name);
      }
    });

    test('a stale wait says so, and says it may already be answered', () {
      final row = controller().rowForTesting(tool({
        'state': 'waiting',
        'activeSessions': 1,
        'waitingTooLong': true,
      }));
      expect(row, startsWith('🟠'));
      // The state text is unchanged — it is still WAITING (protocol.md
      // section 5), and section 10 wants the meaning in the text.
      expect(row, contains('needs you'));
      expect(row, contains('may already be answered'));
    });

    test('mentions session count only when it is worth mentioning', () {
      final one = controller()
          .rowForTesting(tool({'state': 'executing', 'activeSessions': 1}));
      final many = controller()
          .rowForTesting(tool({'state': 'executing', 'activeSessions': 3}));
      expect(one, isNot(contains('session')));
      expect(many, contains('3 sessions'));
    });
  });
}
