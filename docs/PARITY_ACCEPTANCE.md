# VoCat / VoWin final acceptance ledger

2026-10-26: implementation preceded the combined software run, as requested.
The [verification report](VERIFICATION_20261026.md) records the completed checks,
source revision, retained failures and skips. Windows runtime code remains the
mature reference, and Linux-specific features are retained. The suite's success
does not complete the hardware/carrier scenarios listed below.

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
and clock task dispatch with a correctly rejected non-idle modem. That refusal
does not establish successful task business execution.

The full objective remains open. Answered and incoming carrier calls, carrier
hold/DTMF/media/recording/playback/takeover, mobile SMS delivery, physical
hotplug, abrupt crash recovery, IPv6/dual-stack data export, successful scheduled
work and external provider delivery still need their live scenarios. Existing
profile management does not establish provisioning/download/delete of a
disposable profile.

Protocol and ownership details remain in [the feature matrix](FEATURE_UNION.md),
[independent access](INDEPENDENT_ACCESS.md), [task restoration](AUTOMATIC_TASKS.md),
[data handover](CELLULAR_DATA_HANDOVER.md), [Host IMS restart cleanup](HOST_IMS_RECOVERY.md),
[RTCP](RTCP.md), [automatic answering](AUTO_ANSWER.md) and [recording](CALL_RECORDINGS.md).
