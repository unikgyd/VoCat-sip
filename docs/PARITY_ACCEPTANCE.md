# VoCat / VoWin final acceptance ledger

2026-10-26: implementation preceded the combined software run, as requested.
The [verification report](VERIFICATION_20261026.md) records the completed checks,
source revision, retained failures and skips. Windows runtime code remains the
mature reference, and Linux-specific features are retained. The suite's success
does not complete the hardware/carrier scenarios listed below.

QDC507 acceptance follows pure Host IMS in the current VoWin implementation.
The deprecated ADB/D4/UAC bridge is excluded from that target. A module-mode
call, USB PCM, audible ADB-routed tones or module SMS must not be credited as
Host IMS carrier media or SMS delivery.

The verification snapshot records the actual Git revision, complete source
hash manifest, toolchains, native dependencies, test commands, skips, failures
and build artifacts. Historical green snapshots cannot satisfy this ledger.
Changes required by a real failure are tracked against that frozen input and
included in the final candidate; tests are not weakened to manufacture a pass.

| Requirement | Required evidence | Current evidence |
| --- | --- | --- |
| Per-SIM cellular/Wi-Fi intent and default routing | Migration/default tests, both controllers, route-only edits preserving an existing call, actual UI controls | Software suite passed; scenario-specific/live acceptance remains bounded by the report |
| Startup, SIM removal/replacement, USB generations and modem isolation | Independent workers, original identity fences, uncertain metadata handling, cancellation and two-device fixtures | Software suite passed; scenario-specific/live acceptance remains bounded by the report |
| RF, NAS and data ownership | Original SIM rechecks, late workers, acknowledged cleanup, retained unknown starts, no foreign StopAny/default route | Software suite passed; scenario-specific/live acceptance remains bounded by the report |
| Host IMS startup and restart cleanup | Live-owner selection, metadata RF enable, journal/process locks, original network/security/PDN cleanup, changed/unknown record rejection | Software suite passed; scenario-specific/live acceptance remains bounded by the report |
| Host IMS and Wi-Fi coexistence | Separate readiness, separate reconnects, no legacy reset/UIM takeover under Host IMS, per-card policy projection | Software suite passed; scenario-specific/live acceptance remains bounded by the report |
| Web and USB module calls | Dial/answer/hangup, exact original call/physical media owner, no default sound-card fallback, signaling-only maintenance calls | Software suite passed; scenario-specific/live acceptance remains bounded by the report |
| Advanced call controls | Established dialog hold/resume, cancellation/late responses, negotiated RFC 4733, authenticated/deduplicated SIP INFO | Software suite passed; scenario-specific/live acceptance remains bounded by the report |
| RTCP | SR/RR/SDES/BYE framing, loss/sequence/jitter/RTT, endpoint/SSRC checks, actual separate ports, atomic routes, retained socket teardown | Software suite passed; scenario-specific/live acceptance remains bounded by the report |
| Automatic answering and playback | Per-SIM settings/import bounds, ACK-confirmed first answer, exact media lease, browser takeover, cancellation/timeout | Software suite passed; scenario-specific/live acceptance remains bounded by the report |
| Recording | Original PCM identity, stereo/timeline, retention/capacity/private paths, interrupted files, download/delete and error isolation | Software suite passed; scenario-specific/live acceptance remains bounded by the report |
| Scheduled tasks | Line exclusivity, no replay of uncertain calls/SMS, durable journal/restart restoration, profile identity/revert, Host IMS handover, queued refill | Software suite passed; scenario-specific/live acceptance remains bounded by the report |
| Data export and management | Confirmed session leases, marked/interface-bound HTTP/SOCKS/DNS/IP queries, transfer cancellation/join, stale-result rejection, coexistence UI | Software suite passed; scenario-specific/live acceptance remains bounded by the report |
| SIP extension/peer calls and SMS | Registrar/auth, branch ownership/first answer, G.711/IMS bridging, fixed SIM permissions, SMS deduplication/receipts/replies | Full software suite passed; external interoperability remains separate acceptance |
| Notifications and administration | Per-call notification identity, per-channel failure isolation, Telegram ownership, eSIM/USSD/APN and policy boundaries | Full software suite passed; external interoperability remains separate acceptance |
| Frontend and language navigation | Full frontend suite, TypeScript/Vite, actual component states, English README with Chinese switch | 79 frontend tests and TypeScript/Vite passed; English/Chinese navigation present |
| Shipping candidate | Full Go suite, relevant race/cancellation loops, vet, supported embedded-Web Linux builds, installation/rollback instructions | Native/static suites, selected race checks, vet and four builds passed |
| Hardware/carrier acceptance | Exact modem/firmware/library matrix, original and final state, explicit real-service outcomes and unresolved limits | Not proven by unit tests, loopback or compilation |

The software run uses isolated fixtures and build directories. It must not
silently replace the current service, reset physical modems, modify production
Asterisk, or send real calls/SMS to an arbitrary recipient. Tests needing actual
carrier actions require an authorized destination and a controlled window.
Hardware-only skips remain visible; an environment-limited skip is not a pass.

## Live additions — 2026-10-27

The [first NAS run](VERIFICATION_20261027_LIVE.md) proved simultaneous physical
Host IMS/VoWiFi registration, one authorized ringing call, acknowledged shutdown
and controlled environment rollback. It did not prove answered voice or SMS.

The [deployed-service follow-up](VERIFICATION_20261027_FOLLOWUP.md) adds real
Baresip PCMU/PCMA bidirectional decoded audio, RFC 4733 and RTCP, LAN MESSAGE
confirmation/deduplication, original QDC507 USB PCM I/O and WAV/MP3/WMA/AAC
import/preview. Each result is bounded by its actual endpoint and media evidence.
The eSIM run also exposed post-commit metadata lag. After the fix, the final
candidate completed an actual existing-profile round trip with verified HTTP
200 results in both directions. The initial failure and restoration remain in
the record rather than being counted as passes.

The [data and shutdown run](VERIFICATION_20261027_NETWORK.md) adds real CTNET
WDS data, independent authenticated HTTP CONNECT/SOCKS5 HTTPS transfers, observed
modem-interface DNS/TCP traffic, proxy-disable cancellation, public-IP cache
invalidation and original host-routing restoration. It also adds actual manual
and clock task dispatch rejected because of CLCC entries. A later classification
fix identified those entries as non-voice connections; the original refusal does
not establish an active voice call or successful task business execution.

The [IPv6/dual-stack run](VERIFICATION_20261027_IPV6.md) adds real IPv6-only,
simultaneous IPv4/IPv6 and IPv4 regression HTTPS transfers, complete per-run
cleanup, family-fallback repair, isolated kernel ownership cases and native
service/container permission probes. The matching patched ModemManager retained
its original process through data tests and inhibitor release; the distribution
binary's earlier crash remains a separate dependency limitation.

The [native installation run](VERIFICATION_20261027_NATIVE.md) adds actual
systemd installation and failed-upgrade rollback, retained administrator/shared
locks, positive IPv6 public-IP API acceptance and IPv4 regression. The final
frozen source passed 2,062 Go tests, vet, 20 selected race executions and two
Linux builds. Release transport was a controlled checked-build fixture and the
database was an existing private copy; empty-database bootstrap and Internet
GitHub delivery remain outside that run. Automatic data reconnect was confirmed
after a transient startup ownership guard, without another start submission.

The [isolated Docker crash run](VERIFICATION_20261027_CRASH.md) adds actual
SIGKILL/exit-137 evidence and automatic connection restoration using the original
EC20/DITO SIM. The saved intent restored real tunnel/IMS readiness in 28.756
seconds under a new process and bridge network namespace. Docker released the
old namespace's resources; this does not establish cleanup of a surviving host
namespace or recovery of active Host IMS/module-data resources. All original
host/database/container state was restored. Runtime code was unchanged.

The [QMI ownership inquiry](VERIFICATION_20261027_QMI_OWNERSHIP.md) adds 14 real
fresh-client status observations across IPv4, IPv6 and dual stack. It confirms
that readable packet state/runtime does not grant the original lifecycle owner.
The proxy's versioned source retains unreleased client records after application
disconnect. No data crash was injected or counted as recovered; durable bearer
and host-resource recovery remains required. Only a stale source comment changed.

The [ordinary-data fence acceptance](VERIFICATION_20261027_DATA_FENCE.md) then
adds a durable original-owner admission/release fence: 2,066 Linux Go passes,
30 selected race executions, three isolated kernel cases, vet and two builds.
The checked candidate created and released its private record during real CTNET
IPv4, IPv6 and dual-stack connections, with public-IP queries and full original
environment restoration. Process-exit fencing is tested without a live packet
call. Lost-transport automatic cleanup is still open; this is not data-crash
recovery acceptance.

The [surviving-owner acceptance](VERIFICATION_20261027_DATA_OWNER.md) subsequently
adds real host-network main-process SIGKILL and automatic saved-intent recovery
for IPv4, IPv6 and dual stack, with acknowledged original host cleanup before
reconnection. The optional independent owner stayed alive. The corrected candidate
passed 2,076 Go cases, 50 race executions, three kernel cases, vet and four Linux
builds. The initial SQLite-lock failure and transient startup observations remain
visible. Owner-process loss and the native owner unit are not accepted by these cases.

The [Host IMS process-recovery follow-up](VERIFICATION_20261027_HOSTIMS_PROCESS.md)
then passed one real active-main SIGKILL case in the original host namespace.
The original daemon survived; the old PDN/security claim was cleaned before a
new claim and real registration returned in 45.770 seconds without another
enable request. The authorized native IMS mode change was restored to `1,1`,
and both original SIM pairs, profiles, database and host environment were verified.
Transient cleanup failures and two corrected harness assumptions remain recorded.

The [native owner and DNS follow-up](VERIFICATION_20261027_NATIVE_OWNER_DNS.md)
adds actual unmodified native main/owner units, complete IPv4 and dual-stack/IPv4
SIGKILL recovery, real DNS-failure reproduction/repair and independent Windows
HTTP/SOCKS HTTPS. The DNS snapshot passed 2,081 Go cases, 75 race executions,
three kernel cases, vet and four builds. An approved single modem reboot restored
IPv6 reachability while retaining native IMS `1,1`. Its subsequent cold-transport
IPv6 crash failed: the proxy was lost while the original owner remained fenced.
The dedicated proxy layout is implemented; cold live recovery and restoration
of that failed phase remain open. Earlier native passes do not cover this failure.

The [independent native follow-up](VERIFICATION_20261027_NATIVE_INDEPENDENT.md)
then executed the protected proxy unit in a separate real namespace, verified
its survival of a disposable systemd client's actual SIGKILL, and preserved an
already serving host proxy. A fresh database also completed real administrator
bootstrap, authenticated API/static-asset checks and two orderly native service
runs with the original password and session preserved. Hardware/host D-Bus were
explicitly hidden. This closes that scoped bootstrap check; standard empty-host
installer execution, public release delivery and cold carrier recovery remain open.

The subsequent [cold native carrier run](VERIFICATION_20261027_NATIVE_COLD.md)
restored the earlier failed phase through its original owner after an authorized
single recovery reboot. All three unmodified shipping units then passed actual
main-process SIGKILL recovery: IPv4 28.936 s, IPv6 28.883 s, dual stack 29.951 s.
The owner and independent proxy survived with their live transport intact.
Initial IPv6 reachability required a separate maintenance reboot and remains an
environment/dependency limitation. Original host state was restored; an unrelated
container added during the run was preserved. These cases submitted no call/SMS.

The [voice-idle/carrier follow-up](VERIFICATION_20261027_CALL_IDLE.md) fixes that
non-voice classification and adds two successful same-profile registration runs
(manual and real clock), restored original policies and an unchanged other-line
data session. Its final frozen source passed 2,081 Go cases, 115 selected race
executions, three kernel cases, vet and four builds. Actual native main-crash
recovery passed again in all three families. One CT SMS was accepted by the
module, one CT module call rang and cleared, and the original Philippines SIM
completed Wi-Fi IMS registration/shutdown with zero calls/SMS. No SMS delivery
or answered carrier-media result is inferred from these observations.

The [pure Host IMS follow-up](VERIFICATION_20261027_PURE_HOSTIMS.md) removes the
deprecated QDC507 ADB bridge from the target and fixes missing SIM-owned SMSC
reading. Its candidate passed 2,086 Go cases, 120 selected race executions, three
kernel cases, vet and four builds. On the original CT SIM it registered and sent
one IMS SMS, with a later carrier status 0 and `delivered` state. The earlier
module/ADB assisted calls and module SMS are excluded from Host IMS acceptance.
That phase placed no answered pure Host IMS call and observed no recipient reply.

The [Host IMS call and task follow-up](VERIFICATION_20261027_HOSTIMS_CALL_TASKS.md)
adds an answered pure Host IMS AMR call with recipient-confirmed clear tones,
received human PCM and bidirectional RTP. It also records explicit Host IMS
tasks, a retained AT-refresh recovery failure, controlled upgrade recovery and
a separate successful task-preparation SIGKILL recovery. A later real clock
Host IMS SMS task received carrier delivery confirmation; a new six-second
Host IMS call task completed call/signaling cleanup and restored the saved policy.

The [advanced Host IMS follow-up](VERIFICATION_20261027_HOSTIMS_ADVANCED.md)
adds original-line hold/resume, negotiated key submission, an incoming callback
with ACK-confirmed automatic answering, audible prompt playback and same-call
web-phone PCM takeover. Both new calls produced complete stereo recordings with
zero dropped samples and matching receive windows. An isolated checked service
also passed authentication, exact audio/download/range, CSRF, settings and
owned-copy deletion checks using a copy of an actual carrier recording. All
owned hardware/host settings were restored. Runtime inputs were unchanged;
the previous combined software result was not counted as another execution.

The [installed-profile task follow-up](VERIFICATION_20261027_ESIM_TASKS.md)
then fixes ISD-P/ISD-R AID confusion and cold module identity bootstrap behind
the durable recovery fence. The final candidate passed 2,096 Go cases, 15 selected
race executions, three kernel cases, vet and four builds. It recovered the
original failed journal after a controlled upgrade, completed a fresh real
profile registration/revert task, and restored the original profile/preferences
after a separate real post-switch SIGKILL in 84.737 seconds. The interrupted
run stayed failed and was not replayed. Original host state was restored.
The prior AID failure, fixture failure and AID-only cold-recovery failure remain
artifact-specific evidence; no LPA-commit interruption or clock profile dispatch
is inferred from the passing cases.

A separate [clock and EnableProfile boundary follow-up](VERIFICATION_20261027_ESIM_BOUNDARIES.md)
now establishes real scheduled profile dispatch and two distinct main-process
SIGKILL checkpoints. Original-profile recovery was verified in 22.682 seconds
after a complete request write and 84.726 seconds after an observed successful
card response, before UART-reader application consumption. Both interrupted runs
stayed failed without business replay. The same implementation was used, all
1,774 selected input paths still matched, and no new software-suite execution is
claimed. Uninjected observer failures, a card-toolkit-busy task and final original
host restoration are retained separately. Other card/transport failure points
remain untested.

The [fresh standard-installer follow-up](VERIFICATION_20261027_FRESH_INSTALLER.md)
now combines the unmodified installer with an absent database and real native
service startup under an explicit hardware-isolation drop-in. Random first-admin
creation, one-time password output, authenticated access and forced reinstall
preserving the exact administrator/password/session passed. The same checked
binary gained native AMR-NB diagnostic readiness after the installer-recommended
distribution library was installed. Three prerequisite packages were retained,
with no existing package upgrades; the temporary application was cleanly removed
and original modem/network/database/container state was preserved. This does not
cover public GitHub delivery, a clean operating system or first-install hardware
discovery/default subscription behavior.

The [SIP and pure Host IMS follow-up](VERIFICATION_20261027_HOSTIMS_SIP.md)
adds one answered Baresip PCMU-to-carrier AMR call, recipient-confirmed prompt
hearing and speech, and actual handset 5/8 events received by the SIP endpoint.
One authenticated SIP SMS was carrier-delivered and the human OK reply matched
the original-SIM inbox and durable recipient plan with final SIP 200. The actual
callback reached both ringing endpoints but was unanswered after a test-observer
preflight failure; later observation windows had no new callback in that phase.
Arbitrary digit-burst completeness or remote IVR recognition is not inferred.

The [answered callback follow-up](VERIFICATION_20261027_HOSTIMS_SIP_INCOMING.md)
then passed real PCMA incoming answer/media, recipient-confirmed prompt/speech,
received handset key 8 and cancellation of the other ringing extension. The
winning endpoint's 40-second source EOF triggered BYE, and both original calls
cleared. Handset-initiated hangup is not inferred from that teardown. The actual
client WAV and packet decode differ in duration/bytes, with sampled overlapping
audio blocks; lossless recording is not claimed. Original modem/host state was
restored, without new carrier outgoing calls or SMS.

The [ordinary-data proxy-loss follow-up](VERIFICATION_20261027_PROXY_LOSS.md)
adds an actual proxy SIGKILL/exit 137 and automatic proxy restart while the
original owner/main and exact claim stayed retained. The public query returned
409 for unconfirmed data. Controlled original-modem generation change allowed
the same owner to release its own claim/lock, followed by an explicit fresh
data enable and real public query. Final host state was restored. This accepts
fault containment and manual maintenance, not automatic proxy-loss recovery;
owner death and shipping supervision dependency cascades remain open.

The later [opt-in automatic proxy recovery](VERIFICATION_20261027_PROXY_AUTO.md)
adds an independent original-owner transport watch and DMS identity-CID release.
The final frozen candidate passed 2,115 Go cases, 85 selected race executions,
kernel checks, vet and four builds. One real proxy SIGKILL/exit 137 recovered a
fresh-generation claim and IPv6 public query automatically in 61.813 seconds,
with original main/owner retained and no manual reset/restart/new enable after
the fault. Two earlier failed hardware phases and their controlled restoration
remain recorded. This does not accept owner death or default supervision cascades.

The [shipping Compose idle-owner follow-up](VERIFICATION_20261027_COMPOSE_OWNER_IDLE.md)
adds actual default-policy execution, idle owner SIGKILL/exit 137 and exactly one
automatic restart. Main/proxy remained unchanged. A subsequent explicit fresh
data enable used the replacement owner and returned a real IPv6 public query;
full original state was restored. No active claim existed at the fault, so
active-data owner death and proxy supervision cascades remain open. Runtime
inputs were unchanged and the prior combined software checks were not rerun.

The [SIP handset-hangup follow-up](VERIFICATION_20261027_SIP_LIFECYCLE.md)
confirms gateway BYE/endpoint 200 after human handset termination, with other-leg
CANCEL and complete recording. One media route remained until original session
stop; full per-call cleanup failed. The [bounded retry candidate](VERIFICATION_20261027_SIP_CLEANUP_RETRY.md)
passed 2,118 Go cases, 125 selected race executions, kernel/vet checks and four
builds. Its actual deployment registered, but no call arrived in the five-minute
callback window, so repaired per-call resource cleanup remains unaccepted. Physical browser
audio is deferred by human request after a pre-dial missing-device error.

The full objective remains open. Browser microphone/speaker use is deferred
by the human. Repaired SIP per-call resource cleanup,
remote IVR recognition, further SIP call-ending/fault scenarios, physical
hotplug, active-call/daemon/host crash recovery, loss of the independent data owner while it holds an active claim,
unconfigured/default-supervision proxy-loss and other failure combinations,
additional scheduled-task/profile/crash combinations and external provider
delivery still need their live scenarios. Existing
profile management does not establish provisioning/download/delete of a
disposable profile.

Protocol and ownership details remain in [the feature matrix](FEATURE_UNION.md),
[independent access](INDEPENDENT_ACCESS.md), [task restoration](AUTOMATIC_TASKS.md),
[data handover](CELLULAR_DATA_HANDOVER.md), [Host IMS restart cleanup](HOST_IMS_RECOVERY.md),
[RTCP](RTCP.md), [automatic answering](AUTO_ANSWER.md) and [recording](CALL_RECORDINGS.md).
