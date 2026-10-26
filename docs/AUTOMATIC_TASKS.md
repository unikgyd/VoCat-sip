# Automatic task ownership and restoration

2026-10-26 implementation checkpoint. The changes below are **not yet tested
or built**. Final combined verification follows implementation.

A running task holds the selected line's lifecycle writer through preparation,
execution and restoration. HTTP, SIP, Telegram, SMS storage scans and background
radio/data maintenance cannot borrow that line concurrently. Other lines keep
their own gates. Busy operations return promptly instead of waiting behind an
eight-minute task. Read-only call history remains separate from modem I/O.

Task children carry an opaque capability tied to that server, line and live
task. Internal SMS and data work may use it while the writer is held. Another
line or a context retained after task completion cannot use the capability.
Before releasing the writer, the task cancels and joins remaining data workers;
late child I/O keeps the original gate until it actually exits.

Call tasks retain the original call manager and exact call ID. The task waits
for the configured duration or an earlier remote end, requests release of that
call and waits for ended/non-pending signaling before restoring the environment.
The runtime budget includes the requested call duration in addition to the
preparation budget. An uncertain submitted call is not redialed by task retry.

Cellular maintenance calls also work on AT modems without USB audio, provided
they support the guarded call-control/CLCC surface. They use the existing module
call manager's original SIM/physical lease, serial terminal-revision fence,
single-call matching, public call ID and pending-cleanup retention. They do not
advertise USB media, produce synthetic audio recordings or relax web/SIP media
admission. SIP/Web calls continue to require their actual media capability.

Preparation failures preserve their original environment even when execution
never reaches the normal finalizer. A fresh SIM observation chooses the saved
original/target policy. An unrelated replacement card is refused. Requested
profile reversion checks RF-off, verifies the resulting ICCID and treats a late
but verified commit as the actual outcome. Unconfirmed restoration stops task
retry. Independent tasks restore current durable per-SIM preferences rather
than overwriting a newer APN/settings edit with an old snapshot; normal SIM
reconciliation also notices a leftover temporary policy after the task releases
its gate.

Schema 32 adds a private restoration journal before the first access stop,
RF/data change or profile switch. It records original/target card policies,
task-owned device settings and stable hardware evidence. It excludes the
business payload, SIM PINs and proxy credentials. Task/history deletion does not
cascade into cleanup state. Updates and deletion match a random ownership token;
the row clears only after restoration and all task data workers have exited.

Restart recovery fails interrupted running jobs and only restores their
environment. It never repeats a call or SMS. A known original/target ICCID and
the original hardware must be observed; requested profile reversion additionally
requires the saved eUICC EID. An uncertain LPA response is judged by fresh ICCID
readback. Unknown calls, foreign data bearers, replacement cards or incomplete
cleanup keep the recovery record pending. New work on that line returns busy,
and queued tasks wait; other devices keep their independent queues. Full queues
refill from SQLite instead of blocking the fleet scheduler or dropping jobs.

Journaled lines do not auto-enable temporary preferences at startup. The normal
SIM reconciler, legacy maintenance, data work and manual lifecycle writers all
honor the durable fence. The task page shows active or pending restoration;
private journal payloads and tokens are excluded from API responses.

Host IMS tasks observe the original SIM through its metadata owner, acknowledge
IMS/bearer cleanup, and only then expose temporary module/VoWiFi task access.
The task changes only the active independent-access projection; durable per-SIM
preferences stay intact. Restoration merges task-owned fields into the current
device row, recreates inert controller entries as needed and queues the saved
access intent. Carrier registration is still asynchronous. A retained Host IMS
claim cannot be assumed released merely because the process restarted.

Remaining work is the final failure/restart/cancellation, multi-modem, frontend
and build verification, alongside the remaining management integration review.
Real firmware acceptance remains separate from software checks.
