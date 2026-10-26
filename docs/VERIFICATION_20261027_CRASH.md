# Isolated Docker crash/reconnect acceptance — 2026-10-27

The unchanged runtime from `caff83f` was tested with the original EC20 and DITO
SIM. The executed binary's SHA-256 was
`4fea12ecb7153f68c70929878593fc2bb71c7ba87bd48577a5eec383fcb66a8f`.
Its frozen software evidence remains in the
[native run](VERIFICATION_20261027_NATIVE.md); Go, race, frontend and kernel
suites were not repeated for this documentation-only follow-up.

## Actual abrupt termination and reconnection

A separate temporary container used a private copy of the existing database,
shared physical device access and server lock, and a Docker bridge network
namespace. Original cellular auto-start and data remained off. Notifications,
scheduled work and the unrelated SIP gateway were inactive. This scenario
covered real connection recovery without carrier calls or mobile SMS.

The original SIM completed its real ePDG tunnel and IMS registration. The test
confirmed there was no active Wi-Fi IMS call before injecting `SIGKILL` through
Docker. Docker reported **exit 137**, not an OOM termination, with the container
stopped and restart policy `no`. The original Docker sandbox path disappeared;
the host's XFRM state/policy lists remained empty.

The same container was then started once. Its new process and network namespace
were verified, and the saved Wi-Fi intent restored the original SIM's real
tunnel and IMS registration in **28.756 seconds**. No new connection request was
submitted through the API. Both physical equipment/SIM pairs were checked again
afterward.

| Observation | Before SIGKILL | After reconnection |
| --- | --- | --- |
| Confirmed tunnel / IMS readiness | Yes / yes | Yes / yes |
| TUN interfaces in the container namespace | 1 | 1 |
| TUN received / transmitted packets at snapshot | 10 / 17 | 7 / 12 |
| Namespace-local XFRM states / policies | 4 / 8 | 4 / 8 |

These counters are connection evidence, not call media or billing measurements.
The runtime also reported SMS capability ready; no mobile SMS was sent or
delivered as part of this run. Subscriber identities, addresses, cryptographic
material, namespace details, databases and raw logs remain private. The
[machine-readable record](verification-20261027-crash.json) contains the
sanitized outcomes and explicit scope.

## Restoration and retained helper failures

After reconnection, disabling Wi-Fi returned HTTP 202 and reached the controller's
normal `idle` state with enabled, active, tunnel-ready and IMS-ready flags false.
The application then stopped with exit 0. Only the two exact test inhibitors
were released. The original ModemManager PID/restart count, original database
bytes, host modem network, original running container IDs and shared-lock inode
were unchanged. Both original modem/SIM pairs registered with RF on; host XFRM
states and policies remained zero. The temporary test container is stopped.

Two private observers initially made incorrect assumptions: an empty XFRM text
listing was parsed as JSON, and a stopped Wi-Fi controller was expected to use
`disabled` rather than its documented `idle` state. Existing state was inspected
and collection corrected without reconnecting or resubmitting Stop. A cleanup
helper also matched an import marker inside a string before reaching any
mutation. Those helper failures are retained separately from the hardware
outcomes; they are not passing observations or product failures.

## Boundaries and next acceptance

Docker released the old isolated namespace and its kernel resources. This
establishes this container deployment's connection recovery; it does not prove
application journal cleanup while the original host network namespace survives.
The combined Host IMS deployment still requires host networking as described in
[deployment notes](HOST_IMS_DEPLOYMENT.md).

Host IMS needs its own abrupt-exit test with the original ModemManager process,
bearer, host namespace and confirmed journal retained. A read-only preflight
found the QDC507 in its original native IMS mode `1`, registered, with two
non-voice records and no voice calls. Existing profile 5 still provides IMS.
The controlled host-IMS handoff requires temporarily selecting mode `2` and
rebooting that modem, then restoring mode `1` afterward. No firmware write or
modem reboot occurred in this run; that maintenance window was requested
separately because it interrupts the existing connections.

Normal module-data crash recovery also remains open. Source inspection shows
the original host link/MTU/IPv6 settings and installed address ownership held in
the in-memory `ownedNetwork`. A cold IPv6 activation rejects an unowned global
address. The existing orderly shutdown and namespace tests do not establish
durable cleanup/recovery after killing an active host data process. Acceptance
must retain exact original ownership, restore only those host resources and
avoid opening a new client to issue modem-wide StopAny.

Physical hotplug, answered/incoming carrier calls and advanced carrier media,
mobile SMS, successful task business execution, browser work past the first-use
agreement, external notification delivery and disposable eSIM provisioning
remain in the [full acceptance ledger](PARITY_ACCEPTANCE.md). The overall goal
is still open.
