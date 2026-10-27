# Surviving data owner acceptance — 2026-10-27

The optional [independent data owner](DATA_OWNER.md) now has complete real
main-process SIGKILL/recovery cases on the original QDC507/China Telecom SIM for
IPv4, IPv6 and dual stack. Both processes used the **original host network
namespace**, which survived the main process. Docker's on-failure policy restarted
the main service; no new data-enable request was sent after a crash.

This proves main-process recovery while the original data owner survives. It
does not prove recovery after the owner itself, ModemManager, machine or power
fails, nor Host IMS's separate active-process recovery path.

The checked source is based on `e9b5d90`. The
[machine-readable record](verification-20261027-data-owner.json) contains the
frozen source manifest and exact artifact hashes. Both deployed processes matched
the checked amd64 artifact before the process was killed. Private SIM identities,
tokens, PIDs, runtime addresses and credentials are omitted from the public record.

## Combined checks

Implementation preceded the combined checks. The first hardware finding led to
a corrected source snapshot and a fresh complete software run before redeployment.

| Check on the corrected snapshot | Result |
| --- | --- |
| Linux Go suite in private mount/network namespaces | 2,076 passed, 0 failed, 50 environment/opt-in skips |
| Ownership/kernel cases, explicitly enabled | 3 passed |
| Ten owner/delegation cases under the race detector, five runs each | 50 passed |
| `go vet ./...` | Passed |
| Linux amd64 and arm64 builds | Passed |
| Native 386 and armv7 release-script builds | Passed; not executed on hardware |
| Windows targeted device/application packages | Passed |
| Compose override configuration and shared mounts/namespaces | Passed |
| Main/owner systemd unit and drop-in syntax | Passed; owner unit not executed natively |

The suite tests original lifecycle/physical binding, actual child SIGKILL and
flock retention until cleanup acknowledgement, cancellation during Start, another
modem's independence, rejection of foreign descriptors/databases, cold orphan
refusal, close during admission, revocation after server loss, main-side delegation
without opening another WDS owner, and cross-process SQLite WAL visibility.
Final review also preserved local AT data owners and AT-only radio operations
when the optional QMI helper is configured. Two delegation regressions verify
that QMI cannot replace an unreleased AT session and that AT-only radio cleanup
does not depend on an unavailable helper. The final source, including that
correction, received the complete software and three-mode hardware run above.
The existing environment skips remain visible; they are not hardware passes.

## Retained initial failure and correction

The initial candidate passed its software suite but failed the first live
ownership inspection before any active crash injection. The API was connected,
but an external SQLite reader could not see its private claim. Both processes
held deleted WAL/shm descriptors. The new handshake had opened and closed an
extra database-file descriptor to attest that the peers shared the same store.

That direct close can cancel the process's SQLite POSIX locks, as described in
[SQLite's official locking notes](https://www.sqlite.org/howtocorrupt.html#_posix_advisory_locks_canceled_by_a_separate_thread_doing_close_).
The corrected handshake transfers the parent-directory descriptor and compares
file metadata without opening or closing the database inode outside SQLite.
A new regression with an independent SQLite process **fails on the original
candidate** with a lost live WAL and **passes on the corrected source**, including
the race run. The original live packet owner was normally released and the
environment restored before deploying the correction.

The first corrected IPv4 observation attempt also remains retained. Its observer
stopped on an initial `failed` status; read-only inspection then confirmed that
the same container had already recovered automatically. That incomplete
observation is not counted among the three complete cases below. The final
observer watches the existing supervisor and automatic worker until acknowledged
recovery or a bounded timeout, without sending another enable or restart.

## Complete host-network crash cases

| Saved IP mode | Recovery through successful public-IP query | Query result | Initial `failed` status samples |
| --- | --- | --- | --- |
| IPv4 | 26.228 s | HTTP 200, IPv4 | 6 |
| IPv6 | 26.102 s | HTTP 200, IPv6 | 7 |
| IPv4v6 | 27.295 s | HTTP 200, IPv6 | 6 |

The sampling interval was approximately 250 ms. These counts are observations
of startup status, not separate modem failures. Every retained startup error was
`no delegated original data session`; the automatic worker subsequently restored
the saved connection. IPv4/IPv6 each had one supervisor restart. Dual stack had two: after the
intentional exit 137, one startup exited 1 while the surviving owner still held
the host instance lock during cleanup. Docker retried and recovered automatically;
the corresponding lock-refusal log and exit events are retained.
The query result does not claim that every earlier proxy/transfer scenario was
repeated after each crash.

Each complete case verified the precise owned main PID/start time/executable
before SIGKILL, Docker's actual exit-137 event and automatic restart, a new main
process, and the unchanged original owner process. The owner held the original
instance-lock descriptor. The old private claim disappeared, a host snapshot
confirmed the original interface/rules/default routes/settings before reconnect,
and a new claim with a different token appeared for the same original SIM and
USB generation. The host network namespace remained unchanged throughout.

Normal data-off after each recovery again cleared the claim and restored the
original host configuration. A read-only QMI observer confirmed the requested
families during initial connection and both families disconnected after final
release; that observer never started or stopped data and granted no ownership.

## Cleanup and remaining scope

Both final test services exited 0 and the final cleanup completed normally.
An intermediate candidate had a cleanup-script log-directory mismatch; its
restoration resumed from verified stopped services using the original inhibitor
handles. That earlier harness failure remains recorded separately.
The original ModemManager binary/service, registered SIM/equipment
pairs, database, running container IDs and server-lock inode were restored.
No claims or host XFRM state remained. The final copied database passed SQLite
integrity checking. Owned socket and temporary ModemManager files were removed.

No modem reset, firmware write, carrier call or mobile SMS occurred. The native
two-service layout, owner-process loss, machine/power loss, Host IMS crash recovery
and other open carrier/hardware scenarios remain in
[the acceptance ledger](PARITY_ACCEPTANCE.md). The standard installer has not yet
enabled the optional owner layout automatically.
