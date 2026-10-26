# Host IMS application integration

2026-10-24: the prepared access/runtime is now connected to the Linux application,
not just exposed as a protocol API. This is software integration, **not live
firmware/carrier acceptance or a deployed service upgrade**.

2026-10-26 source checkpoint: the independent per-SIM cellular/Wi-Fi policy now
supersedes the single-mode application controls below when `telephony` is set.
See [independent access](INDEPENDENT_ACCESS.md). Its startup/SIM reconciliation,
metadata projection and MM radio-enable changes are **not yet tested or built**;
the verification results below apply only to the older frozen snapshot.

## Per-modem configuration

Each device persists one `ims_mode` (`module`, `vowifi`, `host_ims`) and one
`ims_enabled` flag. Module mode has no host IMS runtime. Empty mode preserves
the older per-card VoWiFi behavior; database migration 28 never silently starts
cellular IMS. Explicit mode is authoritative over legacy card defaults.

The overview provides a mode selector and an explicit Apply action, separate
enable switch, and compact status. Host IMS shows SIM / cellular access / IMS /
SMS, **not an ePDG tunnel**. Green is ready, amber is connecting/stopping, red is
failure, grey is disabled; details are collapsed. A registered voice line does
not require SMS capability to be green. ePDG settings remain specific to VoWiFi.

Authenticated, CSRF-protected API:

```http
GET /api/devices/{id}/ims
PATCH /api/devices/{id}/ims
Content-Type: application/json

{"mode":"host_ims","enabled":true}
```

The response carries `data.mode`, `data.enabled`, and the selected `runtime`
when a provider is available; module mode has no IMS runtime.
Device list/overview add `ims_mode`, `ims_enabled`, `ims_runtime`; legacy WiFi
fields remain separate. Ordinary device edits preserve mode and cannot bypass
the mode transition's cleanup acknowledgement.

## Ownership and dispatch

`hostimsruntime.Manager` creates independent per-device workers. Factory, modem
I/O, callbacks and protocol operations do not hold a fleet-wide lock. Stop joins
startup and late resources; failed cleanup retains the original runtime and
must be retried before a new one is created. Fleet shutdown attempts all lines
concurrently. Calls and media are tied to their creating controller; fixed-card
SMS validation does not trust a stale UI state. An attempted SMS error is not
reported as a preflight "not ready" result, preventing unsafe fallback/resend.

An acquisition error is not proof that no PDN was created. A failed acquisition
with a pending claim returns a non-sendable, cleanup-only capability for that
exact modem, retained by both the pool and runtime. Unknown replies remain
uncertain; only a confirmed original object can be cleaned. Stop cannot ACK or
permit module-mode reuse until cleanup succeeds, and an old cleanup capability
cannot delete a successor claim. Another modem's acquisition/stop is independent.

Before a mode switch or configuration deletion releases a Host IMS line, an
application wrapper also checks durable claim **presence**, even when this
process never prepared the line. A disabled line or an early preparation error
does not erase a previous process's claim. With a saved valid IMEI, only that
equipment's key is checked; foreign claims do not block it. Without a reliable
saved IMEI, only a completely empty claim namespace proves clean. Otherwise the
transition is refused until ownership is resolved. These queries read no private
payload and do not implement cold-start recovery or force deletion.
They do not repair an older database that already mislabeled an unresolved
Host owner as module mode, nor provide a cross-process absence reservation.

The server selects the requested IMS owner for direct telephony, web phone,
SIP carrier calls, SMS, optional USSI and notifications. Explicit Host IMS and
VoWiFi failures do not fall back to AT or a different SIM. Existing SIP call /
message permissions and quotas still apply; an SMS-ready contact is not
permission to send a message. Incoming IMS delivery callbacks are shared by
the access modes, preserving history, subscription identity and notifications.

Mode changes refuse active calls or pending signaling, stop the previous owner,
require a synchronous cleanup ACK, then persist and start the selected mode.
Cleanup failure returns a conflict and does not silently adopt or start another
line. Legacy RF, QMI Sync, eSIM maintenance, data restoration and AT polling are
not allowed to operate under a selected Host IMS owner; switch to module mode
and obtain cleanup confirmation before such maintenance. Pure filesystem
discovery remains available for adding another modem.

The mode API and background legacy I/O share per-device read/write gates and
re-read current policy under the gate. A mode change returns busy rather than
queuing a writer behind nested readers. Adding/editing configuration rejects
duplicate physical ownership (IMEI, USB/control endpoint or resolved device);
two logical names cannot independently operate the same modem. The short
configuration-save lock is not held over modem I/O.

## Linux device binding and startup prerequisites

The factory is inert until a line is enabled. It lazily owns one private system
DBus connection, ModemManager backend, bearer pool and SQLite claim journal.
It matches saved IMEI plus canonical USB/QMI topology to one unique modem;
never a guessed `/Modem/0`, a highest-scoring AT port, or the first match.
Cold startup can use saved strong hardware anchors without first issuing
legacy QMI Sync. The live SIM is then verified by ModemManager and the owned
UIM transaction. Cached AT subscriber fields are not used to select the current
SIM under a Host IMS owner; saved hardware anchors still have to agree.
Background refresh uses the same strict physical resolver; ambiguous ownership
suppresses legacy refresh rather than probing a possibly owned modem.
An unready SIM does not by itself block a different modem: a separate negative
ownership check may prove unique, canonical, non-overlapping USB identities,
non-aliased endpoints and matching current character-device topology, plus USB
epoch checks when the discovered candidate supplies `USBGeneration`.
This is bounded metadata evidence, not a promise of uninterrupted hotplug safety;
missing or contradictory evidence remains blocked.

Current prerequisites remain explicit:

- Supported QMI modem exposed by ModemManager, with an identifiable physical
  USB ancestor and saved hardware identity.
- Valid live SIM and existing regional/device admission checks. The selected
  modem may already be registered (MM state 8 or 11). New source can also send
  `Modem.Enable(true)` for disabled state 3, then observe enabling/enabled/searching
  states 5/6/7 until registration, within the bounded acquisition deadline.
  The unique MM owner and exact hardware/SIM binding are rechecked; connecting or
  disconnecting states remain busy. No modem reset or raw QMI RF write is used.
- Compatible dedicated IMS PDN, WDS/P-CSCF proof, NAS and protected kernel path.
  Unsupported firmware is rejected, not downgraded to unprotected SIP.
  Conflicting existing IMS bearers are refused; this phase does not automatically
  quiesce baseband IMS or take over a foreign bearer.
- AMR-NB bridging additionally needs the host OpenCORE library described in
  [AMR media](AMR_MEDIA.md); cross-compilation alone does not prove codec availability.
- Switching from exclusive VoWiFi RF-off still requires its original cleanup ACK
  before the selected Host IMS owner may enable RF. RF enable follows persisted
  connection intent; stopping Host IMS releases its own bearer and IMS resources
  without treating RF-on as a temporary checkpoint that must be restored.

The factory rechecks the current selected/enabled mode before acquiring new
resources. Stop cleans only owned IMS/access/PDN resources; no modem reset,
foreign bearer deletion or guessed cold-start adoption. A failed/unknown
previous ownership journal still requires explicit recovery, not retries that
erase the evidence. Application admission is not a license grant: original
LICENSE and existing product restrictions remain unchanged.

## Verification and remaining work

Artifacts and exact results live in
[`publish/vocat-application-ims-20261024`](../../publish/vocat-application-ims-20261024).
The final frozen candidate's
[verification manifest](../../publish/vocat-application-ims-20261024/verified-final/verification.json)
confirms unchanged source hashes: Windows Go 1,401 top-level tests passed
(2 skipped), native Linux Go 1,528 passed (43 skipped), and 35 frontend tests
passed, with no failures. Subtests are counted separately in the manifest.
Repeated race runs, TypeScript/Vite checks, Linux amd64/arm64 builds and
`go vet -composites=false` passed. Native service/default-route snapshots are
byte-identical. Skips include disabled system-modem and isolated kernel/XFRM
opt-in tests; those were not rerun and are not claimed as passing here.

Tests use synthetic device inventory, fake private ModemManager, loopback SIP
and isolated UI fixtures. Linux tests run only on `root@10.0.0.2` in a dedicated
build directory, without starting the app service, real QMI/UICC/PDN/REGISTER,
calls or SMS. No WSL, production Asterisk edits or Windows process replacement.

Remaining acceptance: real firmware setup, controlled RF-off to Host IMS
transition, multiple live modems, operator registration/renewal, bidirectional
voice and SMS through both web phone and SIP peers, hardware removal, and
complete cold-start ownership recovery. The new panel is not evidence that
those real-world acceptance items have passed.

Module mode reuses the existing module/AT call and SMS path. This phase does
not add a Linux USB bidirectional audio bridge; web/SIP media bridging still
requires a registered IMS line.
