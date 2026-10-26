# Ordinary-data fence acceptance — 2026-10-27

The new [ownership fence](DATA_OWNERSHIP_FENCE.md) was implemented before its
combined checks and deployment. This acceptance covers the durable fence and
normal original-owner cleanup. Automatic cleanup of a lost WDS transport remains
open; no active carrier data process was killed in this run.

The source snapshot is based on `cba7978`, with manifest SHA-256
`2e4b8cb8926998dcdfa9370d5aad4eb10cc75b85059d3c0493d30cd5e50cf9eb`.
The [machine-readable record](verification-20261027-data-fence.json) contains
the exact build hashes, counts and scoped hardware results. The deployed amd64
binary matched the checked artifact. No subscriber identities, credentials,
runtime addresses, QMI client IDs or handles are included in the public record.

## Combined software checks

| Check | Result |
| --- | --- |
| Linux Go suite, fresh execution in private mount/network namespaces | 2,066 passed, 0 failed, 50 environment/opt-in skips |
| Existing IPv6/dual-stack kernel ownership cases, explicitly enabled | 3 passed, 0 failed |
| Six journal/owner cases under the race detector, five runs each | 30 passed, 0 failed |
| `go vet ./...` | Passed |
| Linux amd64 and arm64 builds | Passed |

The new cases cover original release and independent equipment, duplicate and
malformed claims, conflict with Host IMS, a separate process exiting without
releasing its claim, refusal to acknowledge a cold orphan, journal deletion
retry after an acknowledged original stop, refusal to send Start before durable
admission, and original-client retention after a Start timeout. The process-exit
case uses a private SQLite fixture without a modem or packet call.

The additional two skips relative to the previous 2,062-pass snapshot are
`TestAMRReleaseDefaultHasRealCodec` and `TestNativeAMRAllModesAndRTPDuplex`: the
temporary native AMR package had been removed when restoring the host after the
previous installation run. Existing media acceptance is not counted again.

On Windows, the targeted device, Host IMS and application packages passed.
The store package retained a failure in
`TestStoreConnectionsReapplySafetyAfterPhysicalDiscard/uri=true`: SQLite reported
a ping error for the Windows `file://C:/...` URI. That test and its connection
implementation were unchanged. This is not presented as an all-platform pass.

## Original QDC507 / China Telecom SIM

The new candidate ran in a host-network container using an isolated copy of the
existing application database and its private exact-SIM authorization file.
Original firmware settings and CTNET profiles were retained. Each case used the
application's normal SIM-bound start and original-owner stop paths.

| Requested mode | Private records while connected | Records after stop | Public-IP query | Host configuration after stop |
| --- | --- | --- | --- | --- |
| IPv4 | 1 | 0 | HTTP 200, IPv4 detected | Restored |
| IPv6 | 1 | 0 | HTTP 200, IPv6 detected | Restored |
| IPv4v6 | 1 | 0 | HTTP 200, IPv4 detected | Restored |

The dual-stack case also had connected IPv4 and IPv6 WDS status with valid
runtime parameters. The independent observer only read status/runtime; it never
started or stopped a packet call. It is not a bearer ownership capability.
Each stop cleared the public-IP cache. These are new ownership/connection/query
checks, not a repeat of every earlier HTTP/SOCKS media or export scenario.

The matching temporary ModemManager dependency fix was installed and removed
using the previously verified inhibition procedure. The patched daemon retained
the same PID/restart count during the data cases and port release. The original
daemon binary was restored and active with zero automatic restarts. Both original
SIM/equipment pairs were registered, the original database and running container
IDs were unchanged, the application exited 0, no test claims or host XFRM state
remained, and the original server-lock inode was preserved.

No firmware changes, modem resets, carrier calls or mobile SMS were performed.
Host IMS active-call/crash recovery, ordinary-data automatic recovery after loss
of the original client, physical hotplug and the other open acceptance scenarios
remain separate requirements in [the ledger](PARITY_ACCEPTANCE.md).
