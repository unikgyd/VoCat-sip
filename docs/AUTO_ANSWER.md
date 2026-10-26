# Automatic answering and message playback

Source integration checkpoint, 2026-10-26. These changes have **not** been
tested or built. Implementation precedes the final combined verification run.

The reference is VoWin's `CallExperienceSettings`, incoming-call delay and
`CallMessagePlayback`. Automatic answering is disabled initially. Preferences
and the imported message belong to the live SIM ICCID, not a USB port, device
name, phone number or default outgoing route.

## Operator flow

In a device's Overview, use **Automatic answering and message**. Upload a
message, preview it, choose a delay of 5–120 seconds and save with answering
enabled. Choose either to hang up after playback or wait for browser takeover.
The automatic phase has a 30–3600 second limit, measured from answer, and must
allow at least five seconds beyond the message. Waiting sends silence; it does
not open a server microphone or speaker. The independent [call recording
setting](CALL_RECORDINGS.md) controls whether the original call's audio is saved.

The web phone can take over during playback or while waiting. It requests
microphone permission before submission. The worker stops sending the message
and waits for its downlink reader to exit before transferring the exact call's
audio lease. The new browser must attach within 15 seconds. The automatic time
limit ends at transfer; subsequent disconnect/hangup follows the web-phone
rules. A failed or timed-out transfer can end the original automatic call.

Changing answering settings, replacing/deleting the message, SIM replacement,
USB generation changes, loss of the original call or server shutdown cancel
pending automatic work. Replacing/deleting a message also disables answering.
A new message or newly enabled setting applies to subsequent incoming calls;
an old invitation is not resubmitted. Disabling an in-progress automatic call
ends that owned call. A call already transferred to a browser keeps its browser
ownership.

## Import and persistence

- Uploads are bounded to 64 MiB, one import per server at a time, with at most
  five minutes of normalized audio. The final 20 ms frame is zero-padded.
- The built-in WAV reader accepts mono/stereo PCM 8/16/24/32-bit and IEEE float
  32/64-bit, 4–192 kHz, including supported WAVEFORMATEXTENSIBLE subtypes. It
  validates RIFF/chunk lengths, frame alignment and duration. Stereo is mixed
  to mono; downsampling uses a low-pass filter before interpolation.
- MP3, WMA/ASF and AAC import uses an optional system `ffmpeg`. Input format is
  selected from magic bytes. FFmpeg receives only pipe input/output with a
  `pipe` protocol allowlist, a 20-second deadline, bounded output, limited
  threads and maximum single allocation. A compatible FFmpeg build is required.
  AAC in a streamable MP4/M4A container is also accepted; containers requiring
  seeking may be rejected. The import never treats an upload name as a local
  server path or URL. WAV import needs no FFmpeg installation.
- The normalized signed 16-bit, 8 kHz mono message is held privately in SQLite
  schema 31. No original file or arbitrary playback path is retained.
- Revision comparison and per-SIM storage prevent one tab overwriting a newer
  message/settings revision. SIM identity and USB generation are checked again
  after decoding, before persistence. Preview requires the current ICCID and
  revision and returns canonical WAV with no-store caching.

## Answer and media ownership

Notification delivery is independent of the worker. An invitation is scheduled
once by device ID and exact call ID; there is no number-based call lookup or
automatic answer retry. Before submission, the worker rechecks the original
ringing call, current eligible IMSI/ICCID, physical generation, enabled setting,
revision and stored message. Existing regional restrictions remain in force.

Web phone, Telegram, management call actions, the SIP gateway and the automatic
worker share one answer submission claim. SIP group ringing also uses its
existing local-vs-extension ownership decision. A losing claimant must not
hang up the winner. Only an explicitly known **not attempted** result releases
the submission claim; an uncertain result is not replayed.

The automatic worker reserves the same per-device audio lease used by the web
phone. It waits for an IMS answer ACK or module monitor confirmation of an
active call, plus ready media, before playback. Early media alone is insufficient.
The worker writes 160 samples per 20 ms frame and drains received PCM without
creating another recorder; the media layer applies the recording preference.
Hangup and cleanup name only the original call. The default
outgoing route is never used to relocate an established call.

## Authenticated API

All mutations use the existing management session and CSRF protection.

| Method and path | Purpose |
| --- | --- |
| `GET /api/devices/:id/auto-answer` | Current live SIM settings and compressed-import availability |
| `PUT /api/devices/:id/auto-answer` | Save ICCID, revision, enabled, delay, post-playback and duration settings |
| `POST /api/devices/:id/auto-answer/prompt` | Multipart `iccid`, `revision`, and one `file`; validate/normalize before save |
| `DELETE /api/devices/:id/auto-answer/prompt` | Matching settings ICCID/revision; delete message and disable answering |
| `GET /api/devices/:id/auto-answer/prompt?iccid=…&revision=…` | Authenticated normalized WAV preview |
| `POST /api/phone/takeover` | Exact `device_id`/`call_id`, per-tab client header; return replacement audio lease token |

The phone status reports `audio_owner: auto_answer` and `auto_answer_state`
(`answering`, `playing`, `awaiting_takeover`). Tokens stay in the response and
WebSocket subprotocol, never in URLs.

## Final verification still required

The combined run must cover schema upgrades and revision conflicts; malformed,
truncated, oversized and multi-channel WAV/FFmpeg imports; actual normalized
samples and timing; pre-answer validation; simultaneous SIP/web/Telegram/auto
answers; ACK/module confirmation; cancellation and unknown submissions; exact
SIM/call isolation; physical removal; preview authorization; takeover cancellation,
reader shutdown and missing browser attachment; UI rendering and all builds.
Real USB/IMS calls, listening quality, carrier behavior and actual softphones
remain separate acceptance work. Previous passing snapshots do not verify this
source. The independent call-recording integration also awaits combined
verification in [the feature union checklist](FEATURE_UNION.md).

Format references: [Microsoft RIFF](https://learn.microsoft.com/en-us/windows/win32/xaudio2/resource-interchange-file-format--riff-)
and FFmpeg's [protocol options](https://github.com/FFmpeg/FFmpeg/blob/master/doc/protocols.texi)
and [command options](https://github.com/FFmpeg/FFmpeg/blob/master/doc/fftools-common-opts.texi).
