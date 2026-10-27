# Voice-idle classification and carrier follow-up — 2026-10-27

Both original modules reported non-voice CLCC connections. The line-maintenance
guard treated every CLCC row as a voice call, preventing ordinary policy edits
and scheduled registration. It now uses the module-call subsystem's strict
parser and voice classification. Actual voice calls, malformed/unknown records,
duplicate indexes and unavailable control still block maintenance. Independent
data-owner cleanup remains required before data/access handover.

The [public record](verification-20261027-call-idle.json) identifies the exact
frozen source overlay on `0740914`, manifest and four build hashes. Every live
case below used amd64 artifact
`f8ab0847c407a081da369da87ae667f64a068e056700ae500b5ba31d70d96750`.
The CLI help also places the existing repair flags under `doctor` again.

## Combined checks after implementation

| Check | Result |
| --- | --- |
| Full Go suite | 2,081 passed; 50 visible hardware/environment skips |
| Isolated data kernel cases | 3 passed |
| `go vet` | Passed |
| Data-owner/delegation/DNS race checks, five repeats | 75 passed |
| Selected line/task race checks, five repeats | 40 passed |
| Linux amd64, arm64, 386, ARMv7 builds | All passed |

The first selected server race run exceeded its three-minute package timeout,
after 38 completed top-level executions. Its logs/result remain retained. The
same frozen source, test selection and five repetitions completed in 192.183 s
with a ten-minute timeout; no assertions or test coverage were removed.

## Real native and carrier cases

All three unmodified shipping services were cold-started on the NAS. A separate
diagnostic modem reboot kept native IMS at `1` and the same manager inhibited
before data testing, because the initial IPv6 initialization limitation from
[the earlier cold run](VERIFICATION_20261027_NATIVE_COLD.md) remains unresolved.

| Main-process SIGKILL | Automatic recovery and public query | Address family |
| --- | --- | --- |
| IPv6 | 28.455 s | IPv6 |
| IPv4 | 29.059 s | IPv4 |
| Dual stack | 63.010 s | IPv6 |

Each case retained the original live owner/proxy transport, owner and proxy
PID/invocation, ModemManager, host namespace and same-case original SIM/USB
generation. Original claim deletion and host cleanup preceded a fresh claim.
No enable/restart request followed SIGKILL. Dual stack retained 134 transient
failed-status samples before automatic success; those observations are visible.
Each final disconnect restored the original host state.

One same-profile `cellular_attach` task on the other original SIM completed
both manual and real-clock execution, once each with no retry. Original device,
card and per-SIM policy values were restored, with zero recovery journals; only
the owned task was deleted. The CT data claim, owner and proxy were unchanged,
and its real public query still succeeded. No task call or SMS was submitted.
The observer initially compared JSON text formatting; typed JSON and every
other original field matched. The initial observer failure remains retained,
and neither task was replayed.

On the exact authorized CT SIM, policy enable/disable both returned HTTP 202.
One single-part SMS was submitted once to the controlled recipient: HTTP 202,
one attempted/accepted part, `accepted_by_modem`, delivery unconfirmed. No reply
was observed. One actual module call returned HTTP 202, progressed from dialing
to ringing and was cleared by its original call ID after about 43 seconds.
It was not answered. An earlier HTTP request used a nonexistent API path and
returned 404 before carrier routing; that harness error is retained separately.

The original Philippines SIM completed real Wi-Fi IMS registration, reaching
`sms_ready`, then acknowledged shutdown and restoration of its original policy.
It submitted **zero calls and zero SMS**. Readiness does not establish SMS delivery.

## Restoration and remaining scope

Final attestation confirmed native IMS `1,1`, unchanged original modem profiles,
no remaining voice call/data claim and no pending task/recovery journal. All
three owned services stopped successfully. Temporary native paths and the
patched-manager copy/drop-in were removed. The original manager, both registered
SIM/equipment pairs with RF on, host network, database, lock inode and original
running container IDs were restored.

SMS recipient delivery/reply, answered/incoming carrier media, hold/DTMF,
recording/playback/browser takeover, scheduled call/SMS and task-crash recovery
remain open. These checks also do not establish independent owner/proxy loss,
host/power loss, physical hotplug, browser consent, external notification delivery
or disposable eSIM provisioning. See [the acceptance ledger](PARITY_ACCEPTANCE.md).
