# SIP original-dialog liveness and actual Linux faults — 2026-10-28

The checked Linux build now ends an internal call automatically after either
actual Baresip process disappears. An old-build control stayed active for the
full same 80-second observation. The new build cleared callee/caller faults in
60.412 / 60.275 seconds, notified the surviving endpoint and admitted a new call
without restarting the gateway. This report covers internal Linux calls; actual
carrier-call fault acceptance remains separate.

See [machine-readable evidence](verification-20261028-sip-dialog-liveness.json),
the [earlier 30-second diagnostic](VERIFICATION_20261028_SIP_FOLLOWUP.md) and
[protocol scope](SIP_CALLS.md). Prior failures remain recorded.

## Completed implementation

The baseline is `0d85509c61ad14a69b5c025845027d46fe920cb6`. After the original
answer/ACK becomes active, each accepted SIP leg independently sends a bodyless
OPTIONS every 30 seconds, with a 32-second transaction deadline. At most two
call-owned liveness workers exist; a slow leg cannot delay the other leg's next check.
The original observed UDP destination, Call-ID, both tags, Via and CSeq constrain
the transaction. A timeout or matched 408/481 ends that original call, using its
existing opposite-leg/carrier cleanup. An incoming carrier call starts this only
once its selected SIP owner and original carrier answer are active.

Normal silence does not end a call. Other final responses establish a response
to that exact transaction; their Contact, optional SDP and authentication
challenge do not redirect, renegotiate or trigger credential retries. OPTIONS
and final BYE share increasing local sequence numbers, including after a fixed
peer's authenticated INVITE. Remote request sequences remain independent.
Late answers in other forks keep their own cleanup sequence space.

Normal end and shutdown cancel and join the workers. Tagged inbound OPTIONS
requires the original live dialog; wrong source/tag/sequence is rejected before
transaction-cache allocation, and missing/ended dialogs return 481. This follows
[RFC 3261 dialog request behavior](https://www.rfc-editor.org/rfc/rfc3261.html#section-12.2.1.2)
using [dialog OPTIONS](https://www.rfc-editor.org/rfc/rfc3261.html#section-11).
It does not add RFC 4028 negotiation or media-based inactivity timeouts.

## Frozen implementation, then combined verification

All implementation, documentation and regressions were completed before the
combined run. The final archive contained 1,930 inputs, including the ignored
production frontend assets; every input matched its frozen hash before report
publication. All 868 frontend inputs stayed unchanged, so prior 82 frontend
cases/build are referenced, not rerun. Final manifest SHA-256:
`a70ad09f6f790dd83705bb02061296562cdb8f5bd86198a01923551dac1a787a`.

The first full run passed 2,126 root tests and failed two, with 48 skips. Their
healthy test endpoint expected BYE immediately after replying to one OPTIONS.
The independent worker correctly sent another probe while the other leg was
still reaching its deadline. The fixture was corrected to answer all probes of
the original surviving dialog under one absolute read budget while awaiting
BYE. Only `call_liveness_test.go` changed between frozen v1/v2; product code was
unchanged. That full failed run is retained; no intermediate targeted test runs
were substituted. The correction finished before the complete second run.

| Final combined check | Result |
| --- | --- |
| Full Linux Go suite, CGO disabled | 2,128 passed, 0 failed, 48 skipped; 202.959 s |
| Isolated ordinary-data kernel checks | 3 passed, 0 skipped; 1.571 s |
| Vet | Passed; 6.704 s |
| Recovery and SIP race selection | 245 passed root-test executions over five iterations, 0 failed/skipped; 312.772 s |
| Static builds | amd64, arm64, 386 and armv7 passed |

Go 1.27.3 used pinned offline modules, private mount/network namespaces, isolated
sysfs and `/run`. Regressions exercise silent healthy UDP dialogs, either missing
endpoint, wrong source/tags/Call-ID/CSeq/method/duplicate response headers,
408/481, sequence continuity, cancellation and bounded shutdown, pre-cache
OPTIONS forgery rejection, original incoming/outgoing synthetic carrier cleanup,
and a fixed peer's authenticated INVITE followed by credential-free OPTIONS.
The amd64 artifact used for actual testing is
`796fb281c83990f1fadae7cbbbc498905de21c2061bfdca88564fd5b91c4593e`.
Four full artifact hashes and check timings are in the machine-readable record.

## Actual Linux comparison

Separate old/new fixtures each used a fresh private database, an internal Docker
network and two actual Baresip children with MD5 compatibility and PCMU/PCMA.
The app had a read-only root, dropped capabilities, empty sysfs, no modem/host
D-Bus mounts and no carrier or SMS permissions. Executable hashes and stable app
PID/container identity were checked while running. No carrier call/SMS occurred;
Philippines calls/SMS were zero.

For each fault a pidfd sent SIGKILL once to the exact active softphone child,
after checking executable hash, process start time, original active call and
container membership. Its parent and network stayed alive. The old checked
`d10c0246…` build remained active for 80.255 seconds after callee death, sent no
OPTIONS and needed one explicit healthy-endpoint hangup. That fixture was fully
restored before creating the new fixture.

| New-build actual scenario | Result |
| --- | --- |
| Caller-initiated normal BYE after 70.745 s | Both legs remained active across two default probe rounds; normal end cleared gateway/endpoint slots |
| Callee-initiated normal BYE after 35.662 s | Both legs answered a default probe round; normal end cleared both sides |
| Ringing CANCEL | Propagated CANCEL/200 and cleared both sides; no probes on unanswered dialogs |
| Callee process death | Automatic end in 60.412 s; gateway BYE/endpoint 200 and surviving endpoint slot released; no manual hangup |
| Caller process death | Automatic end in 60.275 s; same original-call cleanup and no manual hangup |
| Fresh call after both faults | Answered, media flowed and normal BYE cleared both sides; gateway never restarted |

Each fault used the full same 80-second observation ceiling, rather than treating
an extended wait as evidence by itself. Captures verify unanswered OPTIONS on
the dead original leg, matched 200 responses on the surviving original leg,
bodyless/credential-free probes and increasing local OPTIONS/BYE sequences.
No endpoint-originated BYE caused either automatic end. Surviving endpoint call
slots were checked before explicitly restarting the dead client.

Their surviving network namespaces emitted 66 / 65 post-fault ICMP unreachable packets in the two
new-build captures. Network tracing again saw ordinary EAGAIN, with no fatal
network syscall. Automatic end therefore did not rely on a fatal media I/O error.
Normal captures contain bidirectional G.711 transcoding and valid separate RTCP;
these packet results do not grade human hearing. Six new-build attempts comprised
five answered internal calls and one ringing cancellation.

## Restoration and remaining scope

Both applications exited zero without restart. All four exact labelled
containers and the internal network of each fixture were removed after archiving
private evidence. Both original modem interface snapshots, policy rules/default
routes, stock manager PID (zero automatic restarts), original user container IDs,
root database hash and shared lock inode were preserved. No XFRM resources
remained. A final read-only check also confirmed both original equipment/SIM
identities registered with RF on and the stock manager executable unchanged. No modem maintenance, native IMS change or paid carrier operation was
performed by these hardware-hidden fixtures.

Actual new-build Host IMS/VoWiFi calls with a crashed SIP endpoint still need
their own live scenario; carrier cleanup here is backed by software regressions,
not substituted with internal-call evidence. Actual no-RTP silence, peer restart
with a prior live dialog, network/host failures and other combinations remain
separate. Physical browser audio stays deferred at the human's request. The
[full acceptance matrix](PARITY_ACCEPTANCE.md) and overall goal remain open.
Raw packets, credentials, addresses, subscriber data and private logs stay outside
Git; only sanitized results are committed.
