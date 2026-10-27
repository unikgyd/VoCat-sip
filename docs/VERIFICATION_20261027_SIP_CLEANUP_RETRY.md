# Bounded IMS media-route cleanup retry — 2026-10-27

The [actual SIP handset-hangup failure](VERIFICATION_20261027_SIP_LIFECYCLE.md)
exposed a retained original media route after the call ended. The cleanup worker
previously made one pass; a rejected route handle then waited for explicit
session teardown. Its initial rejection's exact cause was not instrumented.

## Completed implementation and combined checks

Each media cleanup worker now makes at most three passes, separated by 500 ms
and 1 second, under one shared 15-second budget. Each pass attempts at most 32
retained handles. It neither opens another route nor starts another call, and
does not depend on a browser or another status request. Original-owner authority
checks still run inside the retained capability's Close. Unconfirmed handles
remain fenced after exhaustion; later explicit teardown can retry them.

Three new regressions cover automatic recovery after the first original-route
rejection, bounded persistent failure with the original capability retained for
explicit drain, and a newly retired route arriving during cleanup. Existing
deadline, concurrency, RTCP and ownership cases remain in the full suite.

Implementation was completed and frozen before the combined run. The baseline
is `d8781b43f95fd8b2d99f834478103c21a6e5fbcf`, plus this implementation. All 1,924
archive inputs matched their frozen hashes at the source audit. Manifest SHA-256:
`53eb9a747da42e94dc49d0b0353509206e69c8847f7d9635a43cd54a36e86e2c`.
The [machine-readable record](verification-20261027-sip-cleanup-retry.json)
contains complete check timings and four artifact hashes.

| Combined check | Result |
| --- | --- |
| Full Linux Go suite, CGO disabled | 2,118 passed, 0 failed, 48 skipped |
| Isolated ordinary-data kernel checks | 3 passed, 0 skipped |
| Vet | Passed |
| Selected recovery/media cleanup race checks | 125 passed executions over five iterations, 0 failed/skipped |
| Static Linux builds | amd64, arm64, 386 and armv7 passed |
| Frontend | All 868 inputs unchanged; prior 82 cases/build referenced, not rerun |

The run used Go 1.27.3, pinned offline modules and private mount/network
namespaces with isolated sysfs and `/run`. No intermediate targeted test runs
were made between implementation edits. The checked amd64 artifact is
`d10c0246324e1084d2a24853521a3272785c2e90f9b4ca51acad82de04c3f10a`.

## Actual deployment, pending call acceptance and restoration

That exact artifact was deployed against the original CT pure Host IMS fixture
and registered successfully. Two real Baresip endpoints registered, with carrier
permissions restricted to the original CT line and SMS disabled. The initial
optional IPv4-table observation failed because this IPv6-only fixture has no
IPv4 FIB table; the observer was corrected to select from existing tables before
business began. This was a fixture observation failure, not a carrier call.

No callback arrived during the bounded five-minute observation. This candidate
therefore made no carrier call/SMS and has **no repaired per-call hardware
acceptance**. The earlier failure is retained; actual retry count was not traced.
The prepared endpoint-death scenario was not executed. A real answered SIP call
must still prove original journal and kernel-route restoration before stopping
the whole IMS session.

Owned endpoints/gateway and Host IMS were stopped normally. The original native
IMS/PDP, SIM registrations, stock manager, host network, database, lock and running
containers were restored and separately audited. Physical browser audio remains
deferred at the human's request because the computer is used remotely.

One owned rollback `mmcli` inhibitor survived its earlier SIGTERM. After exact
PID/start/command verification, a bounded termination retry still needed SIGKILL
for that helper alone. Final auditing confirmed no owned inhibitor remained,
both original SIMs registered, and the stock manager stayed at zero restarts.
