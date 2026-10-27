# Pure Host IMS calls and tasks — 2026-10-27

The original QDC507/CT SIM completed an answered **pure Host IMS AMR call**.
The recipient confirmed clear test tones and speaking. Host-owned RTP carried
audio in both directions. No ADB/D4/UAC voice route was involved. This extends
the earlier [SIM-owned SMSC and carrier SMS delivery result](VERIFICATION_20261027_PURE_HOSTIMS.md).

The [machine-readable record](verification-20261027-hostims-call-tasks.json)
binds each result to its own frozen candidate. The full acceptance objective
remains open.

## Changes and retained failures

The task editor, API and storage now accept explicit `environment=host_ims`
for SMS and calls. Schema 33 preserves existing tasks, run history, sequence
values, explicit profile-revert settings and independent recovery journals.
Host IMS preparation waits for its selected controller and original live SIM;
it does not activate ordinary data or run legacy operator registration after
the host owner starts. USB SIM readers remain VoWiFi-only; data/registration
tasks remain cellular.

Initial combined checks found a database environment constraint and a missing
data-runtime resume before Host IMS activation. Both were fixed. A second run
found an incomplete synthetic SMS-result fixture; its missing submission fields
were corrected. Those failed runs remain recorded separately from passing runs.

The first pure call reached 183/180 and received a final 200 with no body.
The old runtime incorrectly tried to parse a new SDP answer and failed the call.
It now keeps successfully negotiated early media only when the final response
has no body and belongs to the same nonempty To-tag. A different fork, absent
early answer or invalid new final SDP cannot borrow that media. ACK and late
fork cleanup remain in place. Six regression scenarios cover these boundaries.

One subsequent attempt was canceled by the test client after a transient
HTTP 409 `line_busy` observation. The client was corrected to retain the error
and keep observing that same call. A separate attempt then completed the
answered-call test. Failed/canceled attempts were not relabeled as successes.
An earlier enable request also received an explicit 409 during startup; its
unchanged disabled state and empty claims were checked before a new enable.

The first interrupted-task test restarted the application, cleared the old
Host IMS claim, and failed the interrupted run without sending SMS. Restoration
then entered legacy `ATI` probing on this QDC507 and timed out. Its journal and
line fence were retained. An observer also encountered the legitimate temporary
absence of `cellular_runtime`; observation resumed without resubmitting work.

The fix uses the original Host IMS metadata owner for unchanged-profile task
preparation/restoration, after acknowledged call/access/data cleanup. Fresh
equipment/topology/SIM verification remains mandatory. The journal records its
environment. Older journals can use this path only when the surviving exact
task still selects Host IMS with matching original/target card; unknown or
changed cases retain conservative recovery. Profile-switch paths keep their
existing RF/eUICC checks. Regression cases reject all legacy AT/refresh I/O for
the unchanged-card path and retain the fence after a changed SIM.

## Answered call

The deployed call candidate was
`96f20291711f6a5a1a00aa184908f53ac344d0b3126369ebaf3fa9f11ae1cf26`
(v4, based on `93aee1e`, source manifest
`f99233d3d5f456b2844b6606644a95a5648a5eed84d96084ef9424c3b793986d`).
The original equipment, IMSI/ICCID, physical USB device, executable, live claim,
security and explicit Host IMS/default route were verified before dialing the
human-authorized recipient.

The web-phone API accepted one exact call. It progressed through dialing,
early media, ringing and active. The negotiated codec was AMR and its PCM
WebSocket attached with the original call's audio capability. Three faded
440 Hz tones used peak 900 and absolute 20 ms pacing.

| Observation | Result |
| --- | --- |
| Recipient | Confirmed clear tones and speaking |
| RTP | 809 packets sent; 572 received |
| Reported packet loss / send errors | 0 / 0 |
| RTCP | 16 reports sent; 4 received; 0 invalid |
| Received PCM | 91,520 samples; 76,581 nonzero; peak 18,384; RMS 2,036.78 |
| Call end | Original call was already ended when deferred hangup returned 409 |

This proves the scoped synthetic downlink/human uplink check. It is not a
long-duration quality score, browser microphone/speaker test, incoming call,
carrier hold/DTMF, recording/playback/takeover or SIP-to-carrier bridge result.
That candidate retained the earlier provisional public diagnostic despite the
independently observed final 200 and active/media state. The final source also
updates the successful final diagnostic and preserves a media-setup failure
reason; the regression exercises 183, 180 and 200 in sequence.

## Final checks and live restoration

The final candidate (v7) is based on `93aee1e`; its 1,892-file source manifest is
`f7708df78d6324dbc2248c7b312497c09d7d166459e905479847b569c5d9191d`.
Implementation was completed before each combined run. Checks used private
mount/network namespaces with hidden hardware and separate runtime locks.

| Check | Result |
| --- | --- |
| Complete final Go suite | 2,094 passed; 0 failed; 50 skipped; 196.955 s |
| Final cleanup race cases, five repetitions | 5 passed; 0 failed; 64.973 s |
| Isolated data-kernel cases | 3 passed; 1.424 s |
| `go vet ./...` | Exit 0 |
| Linux amd64 / arm64 / 386 / armv7 | All built successfully |
| Frontend | 82 passed; 0 failed/skipped; TypeScript/Vite build exit 0 |

The frontend inputs remained byte-identical to their completed run (868 paths).
The preceding v5 metadata/task candidate separately passed 2,092 Go cases and
30 selected task-race executions; the v4 call candidate passed 2,091 Go cases
and 20 selected call-race executions. These are retained artifact-specific
results, not extra executions attributed to v7. The preceding v6 admission/call
race selection also passed 10 executions.

Final amd64 SHA-256:
`ef2ebfced99cf736e21f890adbaf386a7eee8f8677a6a9925727843bb1ed4ca0`.

On v5, the old interrupted task's original journal was resumed after a controlled
upgrade, without another enable or SMS request. This is upgrade recovery, not a
passing automatic-restart result for the failed v4 candidate. A separate fresh
v5 task was then interrupted by real SIGKILL at a frozen, confirmed transaction-free
preparation checkpoint. Docker restarted it automatically; the failed run's
journal cleared, saved preferences and Host IMS registration returned in
58.963 s, and no SMS was submitted/replayed. Original ModemManager PID/restart
count and USB generation stayed unchanged.

The first real clock SMS task on v5 collided with a transient line operation
before acquiring its task lease. It failed with `line_busy`, with zero SMS
submission. Its evidence was retained and the owned future task removed.
The final source waits for initial lease admission within the task budget,
without consuming a business retry. Only that initial failure carries the
admission marker; busy errors after execution begins cannot trigger replay.
Regression scenarios cover release, cancellation and a business-stage busy error.

On v6 the new real clock task executed once, submitted one IMS SMS and obtained
`delivered` status. Its journal cleared, original settings and Host IMS
registration returned. A later independent six-second call task reached
183/180 and completed CANCEL/487, but its first Host IMS stop returned
`cleanup_incomplete`; the run was conservatively failed. Background recovery
subsequently cleared that journal and restored registration. Its failed run was
retained and the owned task removed without redialing it.

The final source waits, within 45 seconds and the parent task budget, only for
`ErrCleanup` from the same retained Host IMS owner. It retries that cleanup,
never the business action. A positive stop acknowledgement releases the cleanup
fence; deadline and unknown-owner errors keep it. Other access modes retain
their existing stop behavior. Regression cases cover settling, deadline and
unknown-owner outcomes.

A separate new v7 Host IMS call task then completed one attempt with a six-second
limit. The exact original call and pending signaling ended, restoration completed,
the journal cleared, saved preferences and Host IMS registration returned. No
SMS was sent by this call task. This is automatic call-control/hangup acceptance;
answered audio is established separately by the v4 call above.

No recipient `OK` was observed in the original CT IMS inbox before shutdown.
Carrier-delivered SMS and a human-confirmed SMS round trip remain distinct.
The Philippines SIM submitted zero calls and zero SMS throughout this phase.

All five phase applications exited 0 after owned cleanup. A read-only final audit
confirmed empty IMS claims/XFRM state and policy, zero task recovery journals and
owned tasks, native IMS `1,1`, both original SIMs registered with RF on, all
original PDP definitions, raw-IP/link/MTU/rules/defaults/IPv6 settings, the original
ModemManager executable, removal of owned temporary manager files, unchanged
original database/server-lock inode and original running-container IDs. The
private ADB server remained stopped. No QDC507 ADB voice product change was added.

The first post-reset network snapshot was transiently different and stopped the
restorer. A fresh read-only snapshot matched every baseline field. Restoration
resumed from the already-confirmed native IMS=1 state without another modem
reboot. The corrected deployment-helper path was likewise a harness-only change
made before any rollout mutation.

Final compiled/test/frontend inputs still match all 1,754 frozen paths. Public
records exclude private identities, destinations, credentials, SIP traces and
voice recordings. Incoming calls, carrier hold/DTMF/recording/playback/takeover,
actual browser audio, carrier SIP bridging, additional failure/profile/hardware
combinations and an IMS recipient-reply round trip remain outside these results.
