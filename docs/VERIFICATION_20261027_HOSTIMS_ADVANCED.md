# Pure Host IMS incoming calls and advanced media — 2026-10-27

The original QDC507/China Telecom SIM completed an outgoing hold/resume call
and an incoming callback with automatic answering, prompt playback and transfer
of the same call's audio lease to the web-phone PCM client. The recipient heard
the tones before and after hold/resume, and both groups during the callback.
Both calls produced complete stereo recordings. No ADB/D4/UAC route was used.

The [machine-readable record](verification-20261027-hostims-advanced.json)
contains the scoped outcomes and remaining cases. This extends the earlier
[answered-call and task acceptance](VERIFICATION_20261027_HOSTIMS_CALL_TASKS.md).
The full acceptance objective remains open.

## Checked artifact and original line

The deployed amd64 artifact was
`ef2ebfced99cf736e21f890adbaf386a7eee8f8677a6a9925727843bb1ed4ca0`,
from the preceding final combined run and source commit
`99150437d38c0c5c16de7de238da09844e21d949`. Its frozen source manifest is
`f7708df78d6324dbc2248c7b312497c09d7d166459e905479847b569c5d9191d`.
All 1,772 selected implementation, test, contract, frontend, script and Go-module
inputs still match that snapshot. Runtime code was unchanged in this follow-up;
the previous software results were retained without claiming another full run.

Before each call, the original equipment, IMSI/ICCID, physical USB topology,
executable, active owned claim, kernel security and explicit `ct-host` Host IMS
route were verified. One outbound call used only the authorized recipient.
The incoming observer matched that handset's callback on the original line.
No Philippines call or SMS was submitted. External notification channels stayed
disabled. A protocol client exercised the real authenticated web-phone API and
PCM WebSocket; actual browser microphone/speaker use remains separate.

## Outgoing hold/resume and key submission

The outgoing call reached active media. The same attached audio client submitted
one hold, one resume and one digit `5`, without automatic control retries.

| Operation | Observed result |
| --- | --- |
| Hold | HTTP 200; same call active, `held=true`, no pending hold |
| Resume | HTTP 200; same call active, `held=false`, no pending hold |
| Media | Ready after both operations; PCM WebSocket stayed attached |
| Digit `5` | HTTP 200 and `submitted=true` |
| Recipient feedback | Heard tones and reported no issues before/after hold |
| Exact call hangup | HTTP 202 |

Test audio used faded 440 Hz tones at peak 900 and absolute 20 ms pacing.
The client sent 336,000 PCM samples and received 144,640, including 122,571
nonzero samples. Recording submission pauses during hold, so the WAV's recorded
uplink sample count differs from client-written samples. These are different
observation points. The key result proves submission through the negotiated
carrier path; no remote IVR digit-recognition result was collected.

## Incoming automatic answer, playback and takeover

The original CT subscription temporarily used a five-second answer delay, a
six-second private 660 Hz prompt, playback followed by waiting, and a 60-second
maximum call duration. The callback progressed through:

1. Incoming ringing on the original Host IMS line.
2. Automatic ownership and answering.
3. Active media followed by positive `answer_confirmed`.
4. Prompt playback, then `awaiting_takeover`.
5. HTTP 202 takeover of that same call, followed by its new PCM capability
   attaching successfully.

After takeover, the client sent faded 440 Hz tones. The caller confirmed hearing
both groups. The client received 50,240 PCM samples, including 34,119 nonzero
samples, after takeover. The final call snapshot was ended with codec AMR:
1,553 RTP packets sent, 424 received, reported loss 0 and send errors 0.
It sent nine RTCP reports and received none; no measured RTT is claimed.
Exact-call hangup returned HTTP 202.

The caller was invited to press `8`, but no explicit received-digit observation
was captured. Negotiated `dtmf_digits` describes capability, not received keys.
The automatic worker's playback and the protocol client's takeover were tested;
a human-operated browser takeover button was not part of this check.

## Recording evidence and management API

Both new recordings matched their exact call, original ICCID, direction and
`host_ims` access. They finished with `state=complete`, `stop_reason=call_ended`
and zero dropped samples. WAVs are stereo, 8 kHz, signed 16-bit; the left channel
contains sent PCM and the right contains received PCM.

| Recording | Duration | Recorded uplink / downlink samples | Receive windows matched |
| --- | --- | --- | --- |
| Outgoing hold/resume | 43.54 s | 272,320 / 145,120 | 18 / 18 |
| Incoming playback/takeover | 33.72 s | 248,480 / 62,560 | 7 / 7 |

The incoming left channel contains three 660 Hz prompt bursts followed by three
440 Hz takeover bursts. The outgoing left channel contains tone groups before
and after hold. Exact comparison used nonquiet 64-sample receive windows selected
every second against the WAV right channel; this is sampled waveform evidence,
not a claim of full-stream equality. Both new WAVs and metadata were mode 0600.

This follow-up also inspected the actual WAV from the preceding answered v4
carrier call. That recording lasted 17.76 seconds, had zero dropped samples,
contained the three source-tone bursts and matched all seven selected receive
windows. Its observed permissions follow earlier private-file maintenance and
are not evidence of its original creation permissions.

A disposable instance of the checked final artifact then exercised the real
recording API using a private copy of that actual carrier WAV and metadata.
Hardware, host D-Bus, host PID and host networking were absent. The root
filesystem was read-only, all capabilities were dropped, and the temporary port
was published only on loopback.

The API denied unauthenticated audio with 401, listed the exact call/SIM/access,
returned bit-identical authenticated audio and downloads, served exact middle
and suffix ranges with 206, rejected an invalid range with 416 and an unknown
recording with 404. Missing CSRF rejected settings changes and deletion with 403
without mutation. Valid settings restored successfully. Deleting only the owned
copy returned 200 and subsequent access returned 404; the original recording was
unchanged. The application exited 0 without hardware or carrier operations.

The first disposable harness used a nonexistent `urllib.request.open` method
before exercising unauthenticated access. That failure was retained, its
application exited 0 and originals were unchanged. The corrected v2 used a fresh
private copy and completed the API checks above.

## Restoration and remaining cases

The temporary prompt was removed and all original answering preferences were
restored. The live application acknowledged owned Host IMS cleanup and exited 0.
The final read-only audit confirmed:

- Native IMS `1,1`; both original SIMs registered with RF on.
- Original PDP definitions and raw-IP/link/MTU/rules/defaults/IPv6 settings.
- Empty IMS claims and XFRM state/policy; zero task journals and owned tasks.
- Original ModemManager executable, zero automatic restarts of the restored
  service, and removal of the owned temporary manager files.
- Original database hash, server-lock inode and running-container IDs preserved.

The initial post-reset network snapshot transiently differed from baseline and
stopped restoration. A fresh read-only snapshot matched all guarded fields.
Continuation verified the already-restored native IMS `1,1` and finished without
another firmware write or reboot. The initial protected stop remains recorded.

Actual browser devices, remote IVR digit recognition, carrier SIP bridging,
recipient-confirmed IMS SMS replies, active-call/daemon/host crash recovery,
physical hotplug, independent data-owner loss, additional task/profile/hardware
combinations and external-provider delivery still require their own evidence.
Private identities, destinations, credentials, SIP traces and audio remain
outside Git.
