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

## Initial deployment window and restoration

That exact artifact was deployed against the original CT pure Host IMS fixture
and registered successfully. Two real Baresip endpoints registered, with carrier
permissions restricted to the original CT line and SMS disabled. The initial
optional IPv4-table observation failed because this IPv6-only fixture has no
IPv4 FIB table; the observer was corrected to select from existing tables before
business began. This was a fixture observation failure, not a carrier call.

No callback arrived during the bounded five-minute observation. This initial deployment
therefore made no carrier call/SMS and provided **no repaired per-call hardware
acceptance in that window**. The earlier failure is retained; actual retry count was not traced.
The prepared endpoint-death scenario was not executed in that initial phase.
The subsequent answered hardware result is recorded below; this initial
no-callback window remains part of the evidence.

Owned endpoints/gateway and Host IMS were stopped normally. The original native
IMS/PDP, SIM registrations, stock manager, host network, database, lock and running
containers were restored and separately audited. Physical browser audio remains
deferred at the human's request because the computer is used remotely.

One owned rollback `mmcli` inhibitor survived its earlier SIGTERM. After exact
PID/start/command verification, a bounded termination retry still needed SIGKILL
for that helper alone. Final auditing confirmed no owned inhibitor remained,
both original SIMs registered, and the stock manager stayed at zero restarts.

## Subsequent answered hardware acceptance

The [2026-10-28 local-date follow-up](VERIFICATION_20261028_SIP_FOLLOWUP.md)
used the same checked artifact without product changes. A real answered CT
pure Host IMS callback confirmed PCMA/AMR media, digit 8, handset termination,
winning-leg BYE/200, other-leg cancellation and complete recording with zero
dropped samples. The original claim journal exactly matched its pre-call value
before session stop; its last media route disappeared about 1.8 seconds after
the first ended API sample. The kernel held exactly the four original journal
signaling routes, with no media host route. The pre-call kernel table snapshot
was unusable because its observer compared string table identifiers with an
integer; this limits literal before/after kernel snapshot comparison. Retry
attempts themselves were not instrumented. Native IMS 1/registered and the full
original environment were restored. Separate actual Linux endpoint-death cases
still require explicit cleanup and are not accepted as automatic recovery.
