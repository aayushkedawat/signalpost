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

    test('flags a wait that has gone on too long', () {
      final row = controller().rowForTesting(tool({
        'state': 'waiting',
        'activeSessions': 1,
        'waitingTooLong': true,
      }));
      expect(row, contains('needs you'));
      expect(row, contains('a while now'));
    });

    test('waitingTooLong annotates rather than changing the state', () {
      // protocol.md section 5: it is emphasis, not a seventh state.
      final urgent = controller().rowForTesting(tool({
        'state': 'waiting',
        'activeSessions': 1,
        'waitingTooLong': true,
      }));
      expect(urgent, startsWith('🔴'));
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
