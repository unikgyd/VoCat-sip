# eSIM clock dispatch and EnableProfile recovery boundaries — 2026-10-27

The original EC20/eUICC completed a real clock-dispatched profile registration
and reversion task. Two separate SIGKILL cases then restored the original profile
and preferences: one after the complete UART request write, and one after the
card's successful response was copied into the application read buffer, before
the UART reader returned to application code. The interrupted runs stayed failed;
no business action was replayed after restart.

The [machine-readable record](verification-20261027-esim-boundaries.json) includes
retained unsuccessful observations, simulator checks and final restoration.
The full live acceptance objective remains open.

## Candidate and exact scope

These cases used unchanged implementation from `e73156c`, with amd64 SHA-256
`5e9ab66648d0442fb4a0bb46c088c009f217cad8c08553b3adc4ac2432bf7044`.
All 1,774 implementation/test/frontend/script/module paths still match the
[previous frozen candidate](VERIFICATION_20261027_ESIM_TASKS.md). Its 2,096 Go
passes, 15 selected race executions, three kernel cases, vet, four builds and
preceding 82 frontend cases are referenced evidence, not new executions here.

Every task selected the original equipment, SIM, USB binding, logical line and
known original eUICC/profile pair. It used `cellular_attach`, cellular access,
an empty payload, no notification, zero automatic-task retries and reversion
enabled. No call or SMS was requested. No profile was downloaded or deleted.
The other CT line stayed in inactive Host IMS metadata mode; this does not
establish preservation of another active call.

## Observed outcomes

| Case | Result |
| --- | --- |
| Actual future clock schedule | No manual run API request; dispatched two seconds after the due minute; one successful attempt; original profile/EID/preferences restored; 223.997 s including the scheduled wait |
| Complete request-write boundary | Full 66-byte UART write returned; all ten threads stopped before/after that write; only the writer resumed; one SIGKILL before any response consumer resumed; original environment verified in 22.682 s after kill |
| Successful card-response boundary | Card `BF31 result=0`, transport `9000`; UART reader stopped before returning to application code; all eleven threads confirmed stopped; one SIGKILL; original environment verified in 84.726 s after kill |

The write-boundary case proves kernel acceptance of the complete request. It
does **not** establish whether the card accepted that request before the kill.
The response-boundary case separately observed card success. Its durable journal
still said `switching`, and the original device projection was still published
at the frozen checkpoint. This specifically exercises recovery before the
application can publish the successful target switch.

Both crashes emitted original-container `die` with exit 137 followed by `start`,
with a new main PID and restart count +1. No manual restart or new task/enable API
request followed either kill. Each interrupted run retained one failed attempt
with the service-restart error. Fresh inventory and AT observations subsequently
verified the original equipment/SIM, installed profile states and EID. All six
task projection fields and durable telephony/card preferences matched their
before-state; no target card preference row was added.

The response-boundary observer saw ten target EnableProfile writes before the
successful response. The source-based explanation is the existing bounded
card-toolkit-busy retry policy inside the LPA operation; intermediate card-result
responses were not individually retained. Automatic-task attempt count remained
one. This is not a claim that a switch always uses one
UART command or that an uncertain commit is safe to repeat.

## Observation method and retained non-passing cases

Private x86-64 [ptrace syscall stops](https://man7.org/linux/man-pages/man2/ptrace.2.html)
selected the exact target request on the bound UART for the write case. The
successful-response case used a PID-scoped kernel return probe and
[bpftrace's signal helper](https://bpftrace.org/docs/release_025/stdlib#signal)
to stop the original application after matching the successful response.
Neither observer modified memory, registers or UART data. Synthetic PTY checks
covered the write boundary, dynamic threads, response consumption, and complete
and split successful responses. Synthetic checks are not carrier results.

Three earlier observations remain separate from the passing fault cases:

- The ptrace response observer timed out without injecting a kill. The task failed
  after a GET RESPONSE `CME ERROR: 0`, then restored the original environment.
  Timing interference was possible, but the root cause was not established.
- A private observer import replaced its HTTP-response dictionary through a
  namespace collision. That observer was cancelled without a kill; its one
  submitted task completed successfully and restored the original environment.
- Another task returned card-toolkit-busy result `0x05` and restored the original
  environment. Its observer also expected CRLF while the application writes CR,
  so no matching request was observed and no kill was injected.

The observer namespace and request-length errors were corrected privately before
the successful response-boundary case. Product implementation was unchanged.
Every owned run was exported privately before task deletion, which normally
cascades the live run rows. Failed histories were not rewritten as successes.
The earlier write-case Docker event archive was retained immediately; the daemon
no longer returned that old event window when queried later.

## Restoration and remaining acceptance

The application exited 0. A separate final audit verified both original SIMs
registered with RF on, original profiles/states/EID, unchanged USB generations,
CT native IMS `1,1` and PDP definitions, and original link state, MTU, addresses,
rules, defaults and IPv6 settings. Task/recovery/IMS-claim/XFRM state was empty.
The original ModemManager binary was restored with zero automatic restarts of
the restored service; temporary manager files were removed. Original database
hash, server-lock inode/availability and running-container IDs were preserved.
No maintenance modem reboot was requested. The Philippines SIM made zero calls
and sent zero SMS.

A matching patched ModemManager was used during these cases and then removed;
the distribution daemon UAF remains a separate limitation. These two checkpoints
do not cover every internal card/transport failure point. Other eUICCs/readers,
disposable provisioning, active-call/daemon/host failures, physical hotplug,
independent data-owner loss and other task/profile/access combinations remain
open. Actual browser audio devices, remote IVR recognition, carrier SIP bridging,
recipient-confirmed IMS SMS round trips and external provider delivery also need
their own live acceptance. Private identities, profile names, credentials,
destinations, UART traces and observer scripts remain outside Git.
