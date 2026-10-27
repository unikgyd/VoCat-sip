# Automatic task ownership and restoration

2026-10-26 implementation. The combined software run is complete, including
task/profile restoration, durable journal and admission regressions. See the
[verification scope and remaining hardware acceptance](VERIFICATION_20261026.md).

Task cleanup can wait up to 45 seconds within its existing budget for the
same retained Host IMS owner to acknowledge completion. Only its explicit
cleanup-pending result is retried; this does not replay a call or SMS.
Unknown ownership or deadline keeps the line and durable recovery fences.

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

After a module process restart, that fence may leave its in-memory IMEI snapshot
empty. Recovery permits only a bounded `AT+CGSN` identity observation on the
saved original USB/serial/reader binding. A matching IMEI and unchanged generation
must precede access/data cleanup; a full refresh then rechecks hardware and the
known original/target SIM before profile writes. Missing binding or a changed
identity keeps the fence. Host IMS continues to use its metadata owner.

Profile switches record the original storage's ISD-R management AID and EID from
live inventory, including original-profile membership. The profile's ISD-P AID
is not a management channel. Reversion resolves that original storage again, so
old version-1 journals with an erroneous profile AID can recover without guessing
an application or discarding their identity checks. Changed EID, missing profile
or ambiguous storage keeps restoration pending.

Host IMS tasks observe the original SIM through its metadata owner, acknowledge
IMS/bearer cleanup, and only then expose temporary module/VoWiFi task access.
The task changes only the active independent-access projection; durable per-SIM
preferences stay intact. Restoration merges task-owned fields into the current
device row, recreates inert controller entries as needed and queues the saved
access intent. Carrier registration is still asynchronous. A retained Host IMS
claim cannot be assumed released merely because the process restarted.

A scheduled job waits within its runtime budget when an initial line lease is
briefly busy. This waiting does not consume a business retry. Only a failure
before lease acquisition receives the admission marker; errors after business
starts keep the existing uncertain-submission and retry fences. Cancellation
while waiting performs no modem operation.

SMS and call tasks now also accept the explicit `environment=host_ims` choice
in the API and task editor. Preparation uses the normal original-owner cleanup
and verified Profile selection, then applies a temporary Host IMS cellular and
default route. It waits for that controller's registered original ICCID/IMSI
and, for SMS, its SMS readiness. No legacy operator registration or ordinary
packet-data activation runs after the Host IMS owner starts. Legacy devices
receive only a temporary independent-access projection; their saved mode is
restored by the same journal. Profile reversion and interruption recovery retain
their existing identity/cleanup fences. USB SIM readers remain VoWiFi-only;
ordinary data and registration tasks remain `cellular`.

For an unchanged profile already using Host IMS, task preparation and restoration
use fresh metadata from the original Host IMS equipment/SIM owner. They first
acknowledge call/access/data cleanup and do not open a legacy AT session to
re-observe that card. The journal stores the task environment. Older version-1
journals can use this path only when their surviving exact task still selects
Host IMS and the saved original/target card agrees; otherwise conservative
recovery remains in force. Profile-switch paths retain the existing RF/eUICC
identity checks.

The [call and task verification record](VERIFICATION_20261027_HOSTIMS_CALL_TASKS.md)
binds the combined checks, retained AT-refresh recovery failure and subsequent
scoped live results to their candidates. Other firmware, profile-switch, call
and interruption combinations require their own acceptance.

The [installed-profile task follow-up](VERIFICATION_20261027_ESIM_TASKS.md)
records these AID/cold-start fixes, a real registration/profile round trip and
a separate post-switch main-process SIGKILL with automatic original-profile
restoration. Interrupted runs remain failed and business actions are not replayed.
