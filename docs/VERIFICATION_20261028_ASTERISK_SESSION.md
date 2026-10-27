# Fixed Asterisk peer: readiness, long dialogs and process faults

2026-10-28. The completed candidate passed real Asterisk 22.5.2 protocol and
process-fault tests on the authorized Linux NAS. Asterisk was an independent,
unprivileged, network-none container; carrier calls and SMS were memory-only
backends. This establishes the fixed-peer SIP boundary, not a new mobile-network
or handset acceptance. The complete acceptance goal remains open.

The [sanitized evidence](verification-20261028-asterisk-session.json) records
source, helper, binary and image hashes, exact outcomes and retained failures.
Raw logs and private test configuration remain outside Git.

## Product fix and frozen inputs

The first run blocked incoming SMS forwarding to real Asterisk under both
SHA-256 and explicit MD5. Its bodyless OPTIONS used a `To` URI containing the
configured UDP port. PJSIP omitted that port in the 401 response; the unchanged
strict response matcher therefore discarded it, and no MESSAGE was released.

Readiness now sends the logical `To` URI without a port. Request-URI, actual UDP
destination, Via and observed response source retain their exact endpoint ports.
Response matching, credential policy, the shared original deadline, queued SMS
expiry and payload admission are unchanged. The new regression covers both
algorithms and 200/401/407; responses from another socket or with a changed To
port cannot complete the probe, and readiness sends no credentials or SMS body.

This follows the URI-component restrictions in
[RFC 3261 section 19.1.2, Table 1](https://www.rfc-editor.org/rfc/rfc3261#section-19.1.2).
It changes the generated request rather than accepting a modified response URI.

Baseline: `3db21943634f9cfa63f52f3171c62eeca38c5db4`, plus the completed readiness
fix and its regression. All implementation preceded the combined checks.
The archive has 1,935 hashed inputs, including existing ignored embedded Web
assets; manifest SHA-256:
`c284f9010d2f28100cc3385565e65e296ab7fa3f2afaeb728d13339edf3a6599`.
All 868 frontend inputs are unchanged from their accepted test/build snapshot.
The private Go overlay adds real Asterisk fixtures without modifying the frozen
source tree or introducing a production fake carrier/CLI.

## Combined software checks

| Check | Result |
| --- | --- |
| Full Linux Go suite, CGO disabled | 2,131 passed, 0 failed, 48 explicit environment skips; 213.658 s |
| Isolated ordinary-data kernel cases | 3 passed, 0 failed/skipped; 1.369 s |
| `go vet ./...` | Passed; 7.579 s |
| Selected recovery, SIP and SMS-readiness race checks, five iterations | 255 recovery/SIP + 65 SMS-readiness root executions passed, 0 failed/skipped; five iterations per root |
| Static embedded-Web builds | Linux amd64, arm64, 386 and ARMv7 passed |

The Go toolchain was 1.27.3, with pinned offline modules. Software checks used
private mount/network namespaces, isolated sysfs and `/run`, and loopback.
The 48 environment skips remain skips. Frontend tests were not rerun for this
backend-only change. Exact build SHA-256 values are in the evidence JSON.

The initially combined race selection exceeded its package's 10-minute budget
after 311 passing root executions. That batch exited 1 and is retained as an
incomplete check. With no source changes, the final run partitioned recovery/SIP
and SMS-readiness into independent private namespaces, retained every assertion
and five iterations per root, and allowed 20 minutes per package. All 320 root
executions completed. The already completed full suite, kernel cases and vet
belong to the same frozen source; they were retained rather than rerun for a
driver time-budget correction. Exact per-group durations are in the JSON.

## Real Asterisk results

Short interoperability passed with SHA-256 and separately selected MD5:
independent bidirectional Digest MESSAGE and INVITE, exact G.711 PCMA PCM echo,
both BYE directions and Asterisk-originated ringing CANCEL. Bodyless readiness
completed before incoming MESSAGE payload. Fixed peers created no registrar
bindings. There was no implicit digest downgrade.

The longer scenarios use SHA-256 and the production 30-second probe interval
and 32-second transaction deadline; neither value was shortened.

| Actual scenario | Result |
| --- | --- |
| Two simultaneous original dialogs, one in each direction, with no injected audio | Survived 67.108 s; zero RTP packet counters before/after, two matched OPTIONS/200 rounds per dialog; exact PCM echo afterward and independent hangup passed |
| Asterisk killed and promptly restarted, gateway calling PBX | Old dialog cleared automatically in 30.012 s after matched original OPTIONS/481; fresh call passed |
| Asterisk killed and promptly restarted, PBX calling gateway | Old dialog cleared automatically in 30.045 s after matched original OPTIONS/481; fresh call passed |
| Asterisk killed and left absent, gateway calling PBX | Automatic cleanup in 62.043 s; fresh call after replacement passed |
| Asterisk killed and left absent, PBX calling gateway | Automatic cleanup in 62.007 s; fresh call after replacement passed |

Each fault killed only the test's own Asterisk child after checking executable,
exact argv/configuration root and process generation, then waited for that exact
child's exit. Replacements bound the same private endpoint with zero channels;
the original gateway runtime remained alive. Before any manual cleanup, each
case had zero original gateway calls and exactly one original-backend hangup.
No restart, registration, new call or replacement endpoint adopted the old call.
Each subsequent fresh call used a new backend ID and ended normally.

The auditor correlated OPTIONS and responses with the accepted original dialog,
Call-ID, From/To tags, Via branch, method and CSeq. It also verified that probes
contained no authentication. Matched 481 and no-response dialog handling follows
[RFC 3261 section 12.2.1.2](https://www.rfc-editor.org/rfc/rfc3261#section-12.2.1.2).
Actual Asterisk dialogs returned 200 while live and 481 after replacement;
this run did not need authenticated OPTIONS retries.

## Retained first-batch failures

The original completed batch exited 1 and is not accepted:

- Both Digest smoke cases timed out before receiving MESSAGE because of the
  product readiness To-port mismatch described above.
- The old minimal Asterisk fixture omitted `res_pjsip_dlg_options.so`. It received
  in-dialog OPTIONS without responding, so healthy calls timed out. The corrected
  fixture explicitly loads and verifies this module; the 67-second positive
  control proves that the correction responds in an established dialog.
- The initial fault harness simulated a remote-ended event by calling the fake
  backend Hangup API, then expected a count that omitted the gateway's later
  cleanup. Fresh call cleanup assertions consequently failed. The corrected
  harness injects a separate remote-ended event; its automatic fault measurement
  still requires exactly one backend cleanup before any such event.

Asterisk's official [dialog OPTIONS module source](https://github.com/asterisk/asterisk/blob/22/res/res_pjsip_dlg_options.c)
identifies the dedicated handler. Deployments with `autoload=no` must load it
alongside `res_pjsip` and `res_pjsip_session`; otherwise this candidate's
no-response policy will clear the call. This is distinct from out-of-dialog
OPTIONS handling already built into `res_pjsip`.

## Isolation, restoration and remaining acceptance

Both completed fixtures removed their exact labelled containers after owner-token
and container-ID checks. Original five container IDs, running state and restart
counts matched before and after; production `vowin-asterisk` was never configured,
reloaded, originated or restarted. Source and frozen harness hashes matched at
completion. Containers had only loopback, UID/GID 10001, a read-only root,
all capabilities dropped, no new privileges, no host devices/ports and only
private test-binary/evidence bind mounts. All real carrier calls/SMS were zero.
No physical SIM, modem, QMI, host DBus, root application database or native IMS
setting was accessed or changed by these tests.

There were 14 answered local fixed-peer dialogs, two ringing cancels and four
logical SIP MESSAGE deliveries; all telephone identities were synthetic.
PCMA exact PCM is packet/media evidence, not human hearing, AMR or real IMS.
The checked test binary exercises the frozen gateway implementation; the four
release builds were not deployed to production during this isolated phase.

Carrier-bound fixed-PBX faults, real SIP endpoint faults during a carrier call,
prolonged network loss, physical hotplug, active independent-owner loss and other
hardware/task/provider combinations remain in [the acceptance ledger](PARITY_ACCEPTANCE.md).
Browser microphone/speaker acceptance remains deferred at the user's request.
