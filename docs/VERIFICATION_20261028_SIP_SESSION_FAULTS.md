# Registered SIP endpoint restart, no RTP and network loss — 2026-10-28

Registered Baresip endpoint restart now clears its original stale call on the
first matched old-target 404. Actual callee/caller replacement ended their calls
in 28.193 / 28.231 seconds, compared with an 80.189-second unaccepted control.
The same checked new build also kept an actual zero-RTP call alive, recovered
that call's media, cleaned up silent network loss and preserved a fresh call
across the prior retired-call drain window.

See the [machine-readable record](verification-20261028-sip-session-faults.json)
and [previous dialog-liveness implementation](VERIFICATION_20261028_SIP_DIALOG_LIVENESS.md).
These are internal Linux registered-endpoint cases. Real carrier-bound faults,
fixed-PBX restart and the rest of the live matrix remain separate.

## Retained failure and completed repair

The earlier checked artifact `796fb281…` handled process disappearance but did
not handle quick replacement correctly. After the exact callee Baresip child was
SIGKILLed and restarted on the same surviving container/network, its new process
had zero calls while the gateway retained the original active call. Two original
dialog OPTIONS received matched **404** replies; a final explicit BYE later got
481. The call stayed active for the full 80.189-second observation and needed
one explicit hangup from the surviving endpoint. No automatic redial occurred.
That actual failure and the original capture remain retained outside Git, with
sanitized results in this report's record.

The classifier had treated every final response except 408/481 as a liveness
reply. It now also ends the original call for matched 404/410 old-target absence.
[RFC 3261 21.4.5](https://www.rfc-editor.org/rfc/rfc3261.html#section-21.4.5)
and [21.4.10](https://www.rfc-editor.org/rfc/rfc3261.html#section-21.4.10)
define those target-absence responses; ending the pinned call on them is this
application's policy. Original observed source, Call-ID, tags, Via and sequence
validation stay intact. A new registration cannot adopt the original call.
Unsupported-method/challenge/redirect handling, media and probe budgets otherwise
keep their existing behavior.

Regressions cover valid 404/410, forged missing-target responses from wrong
source/tags/Call-ID/CSeq/method or duplicate headers, a surviving valid dialog's
next probe, and a newer registration that must not receive the original call's
probe or cleanup. Implementation and complete regressions finished before the
single combined run; no intermediate targeted runs were used.

## Combined checks of the frozen repair

Baseline commit: `a59b3b5b5be80a79445aa2ba5097ac4621ab8acb`, plus this repair.
All 1,932 archive inputs matched their frozen hashes before report publication.
All 868 frontend inputs were unchanged, including production embed assets;
prior 82 frontend cases/build are referenced, not rerun. Manifest SHA-256:
`960acf3802bded2c726f7d0c44d5ca7e3f4251b169096b2fd6e1294c67ed2280`.

| Combined check | Result |
| --- | --- |
| Full Linux Go suite, CGO disabled | 2,130 passed, 0 failed, 48 skipped; 202.751 s |
| Isolated ordinary-data kernel checks | 3 passed, 0 skipped; 1.268 s |
| Vet | Passed; 6.757 s |
| Recovery/SIP race selection | 255 passed root-test executions across five iterations, 0 failed/skipped; 321.859 s |
| Static Linux builds | amd64, arm64, 386, armv7 passed |

Go 1.27.3 used pinned offline modules and isolated mount/network namespaces,
sysfs and `/run`. The actual new amd64 artifact was
`fdc79ff4358285f8a0ad79ca230f9ef39ce1eb018ce5ffbf27ddacc317d729d0`.
Complete timings and four hashes are in the machine-readable record.

## Actual new-build cases

Each fixture used a fresh private database and two actual Baresip children on an
internal Docker network with MD5 compatibility and PCMU/PCMA. No modem/host
D-Bus was mounted, sysfs was empty, root was read-only and capabilities were
dropped. Carrier lines and SMS permissions were absent. Exact app executable
hash, stable app PID/container and original accepted call were checked.

| Actual new-build case | Result |
| --- | --- |
| Callee process replacement before first probe | Matched original-target 404; automatic end after 28.193 s; surviving caller BYE/200 and slots cleared |
| Caller process replacement before first probe | Same result after 28.231 s; surviving callee notified and slots cleared |
| No inbound RTP/RTCP with SIP responsive | Zero captured G.711 packets for 68.569 s; two matched 200 probe rounds per endpoint; original call and counters stayed active/stable; same call recovered bidirectional media |
| Callee all-UDP loss | Zero callee UDP for the 59.745 s capture window; no OPTIONS response; automatic gateway cleanup in 59.945 s; both endpoints acknowledged original BYE after prompt reconnect |
| Fresh call after faults | Still active 38.320 s after prior ended sample, crossing the 32 s retired-branch grace; media and later normal BYE succeeded |

Replacement targeted only the exact original child using pidfd after checking
start time, executable hash, container membership and original active call. The
parent/network stayed alive. The replacement had no original dialog while the
gateway still held it, so this did not substitute an ordinary process-death test.
The capture verifies original URI/dialog/source and increasing local probe/BYE
sequences. No endpoint-originated BYE or harness hangup caused either automatic
restart-case end. Actual Baresip returned 404; 410 is software-regression scope.

For no-RTP, the controller installed media-only UDP DROP rules in both owned
endpoint namespaces. Open namespace FDs pinned the exact original namespace
inodes and container generations before each operation. SIP and control remained
available. The gateway capture had zero G.711 packets and its media counters were
flat for the interval, while OPTIONS/200 continued. Removing the exact owned
rules restored the same original call's media before a normal hangup.

Network loss used a separate all-UDP DROP to/from the app in only the callee's
namespace. DROP produced a silent loss rather than a dead process/ICMP shortcut.
TCP control and the endpoint process remained alive. After automatic gateway
cleanup the controller promptly restored the path; original BYE retransmission
then cleared the callee too, without a manual hangup. Prolonged disconnection
past the cleanup-retransmission window is a separate scenario.

Each table was absent before creation, structurally verified before deletion,
and its namespace ruleset compared to the original afterwards, ignoring packet
counters/handles. Private endpoint namespaces were used for every rule; host
firewall rules were not edited by the test controller. Five new internal SIP
calls were answered. The prior fixture had three answered observations, including
the retained restart failure. All carrier/Philippines calls and SMS were zero.
Packet/slot results do not grade human hearing or substitute browser audio.

## Restoration and open scope

Both fixtures exited their applications zero without restart, removed their four
exact labelled containers and internal networks, and restored every owned filter.
Original user container IDs, root database hash, shared lock inode, host modem
interfaces, policy rules/default routes and stock manager PID were preserved.
The final read-only audit confirmed both original equipment/SIM identities still
registered with RF on, stock manager executable unchanged, zero automatic
restarts and no XFRM state/policy or owned fixture resources.

Actual carrier-call faults on the new build, fixed-PBX/Asterisk restart, longer
network loss and other hardware/host/data-owner/task/provider combinations remain
in [the acceptance matrix](PARITY_ACCEPTANCE.md). Physical browser audio stays
deferred at the human's request. The overall goal remains open. Raw captures,
addresses, subscriber data, credentials and private logs remain outside Git.
