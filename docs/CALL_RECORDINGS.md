# Call recordings

Implementation checkpoint, 2026-10-26. This source has **not** been tested or
built. The final combined run follows completion of the remaining feature-union
implementation. Existing installations have not been replaced.

## Behavior

VoWin's reference is `CallExperienceSettings.SaveCallRecordings`, the recording
directory and `ImsCallManager`'s retention of the most recent 20 calls. VoCat now
has an application-wide recording service shared by Wi-Fi IMS, Host IMS and
explicit USB modem voice. Recording defaults to enabled, retaining 20 calls,
with a 180-minute per-recording limit and 1024 MiB of audio storage. The settings
page allows 1–1000 calls, 1–1440 minutes, and 16–16384 MiB.

Recording starts when a call connects: after the IMS incoming answer ACK (and
SDP/media readiness), or after the USB call monitor confirms the original call
is active. It does not record ringing/early media. Enabling applies to subsequent
connected calls; disabling stops existing recordings immediately. Existing saved
recordings remain subject to retention until explicitly deleted.

Each file is a standard PCM WAV: 8000 Hz, signed 16-bit, stereo. The **left**
channel contains sent audio; the **right** contains received audio. Hold and
network gaps retain silence on the timeline. Arrival jitter is reconciled over a
short merge window; this is an audio observation timeline, not a forensic
packet-timestamp reconstruction. Separate channels can be mixed during playback
or export by an external audio tool.

## Media ownership and failure isolation

The service observes decoded RTP PCM and successful USB PCM writes/capture. It
does not call `ReadPCM` as another consumer, open another ALSA device, or acquire
another audio lease. Browser, SIP carrier calls and automatic answering use the
same original media, so browser takeover does not split or duplicate recordings.
This does not add recording to SIP-only internal calls without a carrier leg.

The recording identity comes from the creating IMS session's subscription or the
original USB call lease, never a later SIM snapshot or the default outgoing route.
Changing routes or moving/replacing a SIM cannot relabel previous recordings.
Opaque random file IDs avoid filenames derived from untrusted telephone numbers.

Media threads enqueue bounded PCM copies only. A dedicated worker performs disk
work; queues, merge buffers and active recorder counts are bounded. Dropped
samples and errors mark recordings incomplete. Storage exhaustion, permission
errors and duration limits stop recording without hanging up the call or
replaying audio. Reaching the storage cap first deletes the oldest finished
recordings; active recordings cannot be deleted through retention or the API.
If active recordings alone consume the cap, recording stops with a storage-limit
status. The count limit and capacity limit may retain fewer calls than requested.

## Files and recovery

The default directory is `recordings` next to the SQLite database, e.g.
`/opt/vocat/data/recordings`. New directories use mode 0700, WAV and metadata
files use 0600. The root is opened with Go's
[directory-scoped filesystem API](https://go.dev/blog/osroot); HTTP routes accept
only a 32-character hexadecimal recording ID, never a path.

Choose another directory using `VOCAT_RECORDINGS_DIRECTORY` or the
`recordings_directory` key in the server JSON configuration, then restart.
Directory selection is deployment configuration; the web UI displays the actual
directory. With systemd's `ProtectSystem=strict`, a directory outside the data
folder also needs a `ReadWritePaths` override. Containers need a corresponding
writable persistent mount. Changing the directory does not move existing files;
move them while the service is stopped if desired.

Each recording has a WAV and JSON sidecar. Publication follows PCM/header sync.
On restart, unfinished recordings have their frame alignment and RIFF sizes
repaired and are marked **interrupted**, never completed. Even an empty file
created just before a process exit is recoverable. Missing identity metadata is
kept unknown; it is not inferred from the current SIM. Malformed metadata,
unexpected file types, unsupported WAV headers, storage errors or an excessive
directory inventory make recording storage unavailable and are reported in
server logs; calling continues. Keep unrelated files out of this managed folder.
Full filesystem or power-loss durability remains part of final verification.

The recordings directory contains runtime data and is excluded from source and
container build contexts at the conventional paths. It is included in the
default Docker data volume. No audio samples are written to application logs.

## Management and API

Use **Settings → Call recordings** to configure saving and retention. Use
**Web phone → Call recordings** to filter by device, preview, download or delete
finished recordings. Partial/truncated/interrupted states are shown explicitly.
Deleting all finished recordings preserves current active recorders.

All routes require the existing management login; mutations also require CSRF.
Audio supports authenticated range requests for browser seeking. Responses use
`Cache-Control: no-store`. Recordings are not exposed through static assets,
SMS API tokens or unauthenticated sharing URLs.

| Method and path | Purpose |
| --- | --- |
| `GET /api/settings/recordings` | Effective settings, directory, usage, active count and last error |
| `PUT /api/settings/recordings` | Save enabled/count/duration/capacity policy |
| `GET /api/recordings?device_id=…` | List retained and active metadata, optionally filter by device |
| `GET /api/recordings/:id/audio` | Finished recording preview; `?download=1` downloads |
| `DELETE /api/recordings/:id` | Delete one finished recording |
| `DELETE /api/recordings` | JSON `{ "all_completed": true }` deletes finished recordings |

## Final combined verification required

Verify exact PCM/channel content, jitter/gap alignment, hold and final frames;
IMS incoming ACK/outgoing/media readiness; USB activation/partial writes;
SIP/browser/automatic playback and takeover without extra readers; concurrent
calls and SIM replacement; queue/disk failure isolation; quota/count eviction;
startup interruption/empty headers and malformed files; cancellation/shutdown;
authentication/CSRF/path/range controls; component rendering; front/backend and
cross-architecture builds; real hardware audio and listening quality. Historical
test runs do not validate this source.
