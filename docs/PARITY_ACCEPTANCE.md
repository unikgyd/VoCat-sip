# VoCat / VoWin final acceptance ledger

2026-10-26: implementation has been completed in batches before running tests,
as requested. This ledger defines the final combined run; **no result below is
claimed passed yet**. Windows runtime code remains the mature reference, and
existing Linux-specific features remain in scope.

The verification snapshot must record the actual Git revision, complete source
hash manifest, toolchains, native dependencies, test commands, skips, failures
and build artifacts. Historical green snapshots cannot satisfy this ledger.
Changes required by a real failure are tracked against that frozen input and
included in the final candidate; tests are not weakened to manufacture a pass.

| Requirement | Required evidence | Current evidence |
| --- | --- | --- |
| Per-SIM cellular/Wi-Fi intent and default routing | Migration/default tests, both controllers, route-only edits preserving an existing call, actual UI controls | New source; final run pending |
| Startup, SIM removal/replacement, USB generations and modem isolation | Independent workers, original identity fences, uncertain metadata handling, cancellation and two-device fixtures | New source; final run pending |
| RF, NAS and data ownership | Original SIM rechecks, late workers, acknowledged cleanup, retained unknown starts, no foreign StopAny/default route | New source; final run pending |
| Host IMS startup and restart cleanup | Live-owner selection, metadata RF enable, journal/process locks, original network/security/PDN cleanup, changed/unknown record rejection | New source; final run pending |
| Host IMS and Wi-Fi coexistence | Separate readiness, separate reconnects, no legacy reset/UIM takeover under Host IMS, per-card policy projection | New source; final run pending |
| Web and USB module calls | Dial/answer/hangup, exact original call/physical media owner, no default sound-card fallback, signaling-only maintenance calls | New source; final run pending |
| Advanced call controls | Established dialog hold/resume, cancellation/late responses, negotiated RFC 4733, authenticated/deduplicated SIP INFO | New source; final run pending |
| RTCP | SR/RR/SDES/BYE framing, loss/sequence/jitter/RTT, endpoint/SSRC checks, actual separate ports, atomic routes, retained socket teardown | New source; final run pending |
| Automatic answering and playback | Per-SIM settings/import bounds, ACK-confirmed first answer, exact media lease, browser takeover, cancellation/timeout | New source; final run pending |
| Recording | Original PCM identity, stereo/timeline, retention/capacity/private paths, interrupted files, download/delete and error isolation | New source; final run pending |
| Scheduled tasks | Line exclusivity, no replay of uncertain calls/SMS, durable journal/restart restoration, profile identity/revert, Host IMS handover, queued refill | New source; final run pending |
| Data export and management | Confirmed session leases, marked/interface-bound HTTP/SOCKS/DNS/IP queries, transfer cancellation/join, stale-result rejection, coexistence UI | New source; final run pending |
| SIP extension/peer calls and SMS | Registrar/auth, branch ownership/first answer, G.711/IMS bridging, fixed SIM permissions, SMS deduplication/receipts/replies | Existing and new paths; final run pending |
| Notifications and administration | Per-call notification identity, per-channel failure isolation, Telegram ownership, eSIM/USSD/APN and policy boundaries | Existing and new paths; final run pending |
| Frontend and language navigation | Full frontend suite, TypeScript/Vite, actual component states, English README with Chinese switch | Documentation source present; final run pending |
| Shipping candidate | Full Go suite, relevant race/cancellation loops, vet, supported embedded-Web Linux builds, installation/rollback instructions | Final run pending |
| Hardware/carrier acceptance | Exact modem/firmware/library matrix, original and final state, explicit real-service outcomes and unresolved limits | Not proven by unit tests, loopback or compilation |

The software run uses isolated fixtures and build directories. It must not
silently replace the current service, reset physical modems, modify production
Asterisk, or send real calls/SMS to an arbitrary recipient. Tests needing actual
carrier actions require an authorized destination and a controlled window.
Hardware-only skips remain visible; an environment-limited skip is not a pass.

Protocol and ownership details remain in [the feature matrix](FEATURE_UNION.md),
[independent access](INDEPENDENT_ACCESS.md), [task restoration](AUTOMATIC_TASKS.md),
[data handover](CELLULAR_DATA_HANDOVER.md), [Host IMS restart cleanup](HOST_IMS_RECOVERY.md),
[RTCP](RTCP.md), [automatic answering](AUTO_ANSWER.md) and [recording](CALL_RECORDINGS.md).
