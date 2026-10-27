# Ordinary-data QMI proxy loss — 2026-10-27

An actual SIGKILL of the private QMI proxy produced Docker exit 137 and one
automatic proxy restart. The original data owner and main process stayed alive,
but data did not recover automatically: the exact original claim remained and
the public-IP API rejected the unconfirmed connection with HTTP 409,
`cellular_data_unconfirmed`. This establishes real fault containment rather
than recovery based on a replacement proxy's readiness.

Controlled maintenance then succeeded. With the original owner retained, one
authorized original-modem reboot changed its USB generation while native IMS
stayed at setting 1. The original owner deleted its own obsolete claim and
released the original instance lock. No journal was forcibly cleared and no lost
CID/packet handle was adopted. A subsequent explicit new data enable and real
public query passed, followed by a clean disconnect and host restoration.

The [machine-readable record](verification-20261027-proxy-loss.json) separates
the actual fault, manual recovery and remaining automatic-recovery gap. The full
hardware objective remains open.

## Candidate and isolation

The unchanged checked artifact was
`5e9ab66648d0442fb4a0bb46c088c009f217cad8c08553b3adc4ac2432bf7044`.
All 1,774 selected implementation/test/frontend/script/module paths match the
frozen manifest. Only private helpers changed; no new software-suite or build
execution is claimed.

Three private Docker processes shared the host network/PID namespaces, private
database and original lock directories used by the documented owner layout.
All three used the same checked binary through a read-only mount. The proxy
restarted on failure; main and owner automatic restarts were disabled to retain
the original owner through the fault. These are explicit lab policies, not an
execution of the unchanged shipping Compose configuration or systemd dependency
cascades.

The exact original China Telecom equipment/SIM pair was used for ordinary QMI
data. Both modems stayed independently inhibited from ModemManager while the
application owned their ports. Native IMS was read as 1,1 before and after the
case, with zero IMS-setting writes. No carrier voice call or SMS was submitted;
the Philippines SIM made zero calls and sent zero SMS. This phase does not
establish Host IMS voice/session recovery.

## Actual outcomes

| Check | Observed result |
| --- | --- |
| Initial data | Dual-stack configured; original owner claim; real public query HTTP 200 using IPv6 |
| Proxy fault | Exact owned process killed through a PID descriptor; Docker exit 137 and one automatic restart |
| Original main/owner | Same processes through the fault window; exact original claim retained; owner's lock duplicate present |
| Data after proxy restart | Observed failed/starting/recovering phases; no confirmed connection; public query HTTP 409 |
| Maintenance quiescence | Desired data disabled once; main deliberately killed with automatic restart disabled; original owner retained claim and instance lock |
| Original-modem recovery | No other AT opener; original identity and native IMS verified; one CFUN reboot; new USB generation and native IMS registration confirmed |
| Ownership cleanup | Original owner acknowledged old-generation loss, deleted its own claim and released the instance lock |
| Positive follow-up | Main manually started with data disabled; explicit new enable created a new-generation claim on the same SIM and owner; public query HTTP 200 using IPv4 |
| Final disconnect | Acknowledged disabled state, no claim, original host configuration restored |

The owner did not restart anywhere in the case. The main SIGKILL in maintenance
was intentional to quiesce AT access while the original owner retained cleanup
authority; it is not another automatic main-recovery acceptance run. The main's
final orderly shutdown exited 0, as did the owner.

The 37.149-second fault observation includes proxy restart and read-only checks.
The replacement proxy did not resolve the lost original transport. The recovery
required a modem reboot, manual main start and explicit fresh data enable; it
must not be counted as automatic proxy-loss recovery. The post-maintenance
public query used IPv4, so it does not independently prove restored end-to-end
IPv6 under that dual-stack configuration.

## Retained preparation findings

Private helper imports and continuation indentation needed repair before any
fault signal. Existing manager/port acquisition was continued without replaying
the manager switch. No firmware write or injected signal occurred in those
failed preparations.

The original ModemManager-owned proxy did not disappear within the bounded
absence wait, despite having no established client. After verifying those
properties again, the owning temporary manager was stopped normally. Proxy
process/socket absence was confirmed, the private proxy cold-started, then the
manager restarted and the exact original modem inhibitors were re-established.
No serving proxy was adopted or individually killed during preparation.

## Final state and remaining acceptance

All three owned containers stopped. The private owner socket/runtime was
archived; Host IMS/XFRM and ordinary-data claims were absent. Both original
equipment/SIM pairs returned registered with RF on and the original native IMS
setting 1. Stock ModemManager was restored with zero automatic restarts; its
temporary override/executable and all live owned inhibitors were absent.

China Telecom host state matches the baseline except for the interface index
recreated by the authorized reboot. The Philippines host snapshot matches
exactly. Original database hash, shared-lock inode and running-container IDs
were preserved. Private subscriber identities, claims, network addresses,
credentials and raw logs stay outside Git.

Automatic proxy-loss recovery, death of the independent owner, default
Compose/systemd dependency behavior under proxy failure and host/power loss
remain open. Browser audio, remote IVR recognition, further call-ending and
hardware/profile/provider scenarios retain their separate acceptance entries.

The later [opt-in automatic recovery follow-up](VERIFICATION_20261027_PROXY_AUTO.md)
adds new implementation and one successful actual automatic case. It preserves
the failure/manual-maintenance outcome recorded here and the separate owner-death
and default-supervision boundaries.
