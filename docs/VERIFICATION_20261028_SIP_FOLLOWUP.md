# Actual SIP callback cleanup and Linux endpoint faults — 2026-10-28

This follow-up completed across late 27 / early 28 October in Asia/Taipei. It
used commit `4418eefdccee2290a243aa67fc0b791778b7aac2` and the already checked
amd64 artifact `d10c0246324e1084d2a24853521a3272785c2e90f9b4ca51acad82de04c3f10a`.
No product implementation changed. All 1,815 frozen manifest paths outside
`docs/` and root `README*` still matched their hashes; the prior combined
2,118 Go / 125 selected race / kernel / vet / four-build results are referenced,
not rerun. See the [cleanup implementation report](VERIFICATION_20261027_SIP_CLEANUP_RETRY.md)
and [machine-readable evidence](verification-20261028-sip-followup.json).

## Answered CT pure Host IMS callback: passed

Two real Baresip endpoints registered with permissions restricted to the original
CT line and SMS disabled. The authorized handset callback was answered once by
endpoint 2 only after the real caller identity and actual endpoint ringing were
confirmed. The selected original carrier call, SIP call and USB generation
remained bound throughout; the QDC507 used pure Host IMS with native IMS
temporarily disabled. No deprecated ADB/D4/UAC voice bridge was used.

The accepted call used SIP PCMA and carrier AMR. Answer/ACK and bidirectional
media counters were confirmed; the packet capture contains telephone-event
key 8. The human replied that the requested callback, speech, key and handset
hangup were performed. This reply does not independently grade audible quality.

After handset termination, the gateway sent one BYE to the winning endpoint,
which returned 200. That endpoint sent no BYE; the other ringing fork received
CANCEL and returned 200/487. No endpoint hangup was submitted by the harness for
this accepted call. The carrier ended, the SIP call cleared and the complete
14.360-second recording closed with zero dropped samples and both PCM directions.
The captured gateway-to-endpoint PCMA includes 3.940 seconds of decoded payload;
that is payload duration, not the duration of the whole call.

The original private claim journal exactly matched its pre-call value before
stopping the whole IMS session. At 100 ms sampling, the first zero-media-route
sample was 1.794 seconds after the first ended API sample. The host network
snapshot also matched. A fresh normalized full-table query found exactly the
four original journal signaling routes with matching destinations, interface
and gateways, and no extra media host route. This is the repaired per-call
cleanup acceptance missing from the [earlier retained failure](VERIFICATION_20261027_SIP_LIFECYCLE.md).
Actual cleanup retry attempts were not instrumented; this result does not
identify the earlier rejection's exact cause.

The pre-call kernel-table observer compared an integer table number with
iproute2's JSON string and saved empty route arrays. Literal kernel snapshot
before/after comparison is therefore unavailable. The original journal and
post-call expected-route verification above supply the narrower valid evidence.

One earlier callback in the same deployment was unanswered: the private observer
aborted on the legitimate `anonymous` placeholder before the incoming worker
resolved the caller. No answer intent was submitted. The harness was corrected
to wait for the authorized identity and real target ringing; product source was
unchanged. That failed observation and the previous five-minute no-callback
window remain recorded. This deployment observed two incoming carrier calls,
one answered, zero outgoing calls and zero SMS; Philippines calls/SMS were zero.

## Hardware-hidden Linux SIP: three normal paths passed, two faults remain open

A fresh private administrator/database and two real G.711 Baresip endpoints ran
on an internal Docker network. The application had no modem mounts or host
D-Bus, an empty read-only sysfs, dropped capabilities and no carrier/SMS
permissions. It used the same checked executable. These cases establish Linux
endpoint behavior, not a carrier fault or physical microphone/speaker result.

| Actual scenario | Result |
| --- | --- |
| Caller BYE | Both dialog legs exchanged BYE/200 as expected; gateway and endpoint slots cleared |
| Callee BYE | Same result with the callee as initiator |
| Ringing CANCEL | CANCEL/200 propagated to the ringing leg; gateway and endpoint slots cleared |
| Abrupt callee process death | Call stayed active for the full 30-second observation; explicit healthy-endpoint hangup required |
| Abrupt caller process death | Same result; automatic crash recovery not accepted |

Normal answered cases captured bidirectional PCMU/PCMA transcoding and valid
separate RTCP. Subsequent calls were admitted after explicit crash cleanup
without restarting or reconfiguring the gateway.

For each fault, a pidfd targeted the exact original Baresip child after checking
its process start time, executable hash, container membership and original
active call. Only that child received SIGKILL. Its parent container and network
namespace stayed alive. Each capture recorded 36 ICMP destination/port-unreachable
packets from the affected endpoint after death. The gateway stayed running.
Read-only syscall tracing recorded ordinary `EAGAIN`, with no fatal network
I/O error. No BYE from the dead process preceded the remaining active state.
A patch that merely notices fatal media I/O errors is therefore not established
as a solution by these cases. The observation is bounded to 30 seconds, not a
claim about every later SIP timeout. A bounded session-liveness policy and its
fault/hold/silence scenarios remain further work.

Eight internal SIP attempts were made: six answered calls and two ringing
cancellations, including the capture correction below. There were zero carrier
calls or SMS in this isolated phase. Each fault was explicitly cleaned up through
the surviving endpoint before a new softphone process was started.

Fixture observations are retained separately from product results. The first
three normal cases passed their status/control checks, but default capture
buffering lost their tail packets, including all of the very short CANCEL
capture. All three were repeated with immediate capture and a drain delay;
complete termination packets were then verified. A caller-crash preflight also
stopped before dial/kill because a recreated client left an additional still-valid
Contact. The retry validated both synthetic accounts and only the exact owned
source addresses rather than assuming exactly two Contacts. An initial private
bootstrap stdin failure was corrected before service/network creation.

## Restoration and scope

The CT application exited zero without restart. Original native IMS `1,1`, PDP
profiles, both original registered SIMs, stock ModemManager, root database hash,
shared lock, host settings and running container identities were restored. Only
the expected CT interface index changed across authorized modem maintenance.
No owned inhibitor, IMS claim or XFRM resource remained.

The isolated LAN application also exited zero without restart. All seven exact
labelled containers and its internal network were removed after archiving private
evidence. The original user container IDs, database, lock, policy rules and
default routes were preserved. Stock manager PID and both exact interface
snapshots stayed unchanged throughout LAN business. That LAN business baseline
was taken after the separately authorized CT maintenance and restoration;
it is not a claim that the manager PID remained unchanged across modem testing.

Raw captures, credentials, subscriber identities, recordings and addresses remain
outside Git. Physical browser audio is still deferred at the human's request.
The endpoint-death automatic termination cases and the other live combinations
listed in [the acceptance matrix](PARITY_ACCEPTANCE.md) keep the overall goal open.
