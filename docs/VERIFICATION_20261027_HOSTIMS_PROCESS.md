# Host IMS process recovery — 2026-10-27

The original QDC507/China Telecom SIM passed one actual **active Host IMS main
process SIGKILL** case in the original host network namespace. Docker restarted
the main process automatically, and real IMS registration returned in **45.770 s**
without another enable request. The original ModemManager process survived.

The deployed artifact matches the checked implementation in `70fc3b8`.
The [machine-readable record](verification-20261027-hostims-process.json) includes
its exact hash, source manifest, observations and restoration assertions. The
same artifact passed the [combined software checks](VERIFICATION_20261027_DATA_OWNER.md):
2,076 Go cases, 50 race executions, three isolated kernel cases, vet and builds.
This follow-up added hardware evidence and documentation; no implementation changed.

## Authorized maintenance and registration

The human explicitly authorized this original modem's native IMS mode change
from `1` to `2`, its reboot, interruption of two known non-voice connections,
and restoration after testing. Equipment, SIM, physical USB path, existing IMS
profile 5 and absence of voice calls were checked before the write. The modem
confirmed mode `2` and a changed USB generation after reboot. No modem profile
was edited and no carrier call or mobile SMS was submitted.

A matching temporary ModemManager dependency build was selected through a runtime
drop-in; the distribution executable remained unchanged. The CT modem stayed
managed for its IMS PDN. Only the independent original EC20 was inhibited during
application operation. The application used a private database copy, with tasks,
external notifications, ordinary data and Wi-Fi automatic access disabled.

The original CT SIM reached real Host IMS registration and SMS capability before
the crash. This is registration evidence, not proof of SMS delivery or call media.

## Actual crash and automatic recovery

The observer checked the precise main PID, start time and executable, a confirmed
active journal without pending/uncertain transactions, its original daemon/SIM
binding, and live security state before SIGKILL. Docker recorded exit `137` and
one automatic restart into a new main process in the unchanged host network
namespace. ModemManager's PID and restart count stayed unchanged.

The old claim disappeared, with an intermediate empty claim and empty host XFRM
state/policy observation before reconnection. The old PDN was deleted, old XFRM
ReqIDs were absent and the three original security reservations were removed.
A different PDN and claim token appeared for the same original daemon, modem,
SIM, USB generation and binding. The new runtime then completed real registration.
No new enable request, modem reset or manual application restart was issued
between the crash and recovery.

The recovery observations include a transient `host_ims_cleanup_failed` state
and two `access_close` / `outcome_uncertain` warnings. The same automatic worker
subsequently recovered; these observations are retained rather than treated as
terminal failures or omitted from the result.

## Retained harness corrections

The first private setup changed the device projection but left a different
per-SIM saved mode. The reconciler restored that saved mode. The test stopped
the still-disabled application and made the two private records agree before
enabling access; no firmware write was repeated. This setup error is not counted
as a failed crash case.

After the successful case, the stop observer expected `disabled`, but Host IMS
acknowledged the stopped state as `idle`, with `enabled=false`, no active runtime,
no claim and no XFRM. A separate read-only check confirmed that original Stop's
acknowledgement, then the application exited normally. No second Stop request
was sent; the obsolete observer's later connection-refused exception is retained.

## Restoration and limits

The application exited `0`. Native IMS was restored to `1` with another reboot
and positively verified registered as `1,1`. Both original equipment/SIM pairs
were registered with RF on. Firmware profile definitions, the original database,
running container IDs and global server-lock inode matched their baselines.
The distribution ModemManager service/binary was restored with zero automatic
restarts; the owned temporary executable and drop-in were removed. No Host IMS
claims, security reservations or host XFRM remained. The lower interface's MTU,
flags and IPv6 settings, source rules and default routes matched the baseline;
its old netdev index was not required across the authorized hardware reboot.
The private database passed integrity checking.

This accepts one confirmed active-main-process recovery case while the original
daemon and host namespace survive. It does not establish recovery during unknown
mutations, an active carrier call, ModemManager loss, machine/power loss or physical
hotplug. Carrier answer/media, mobile SMS delivery and other outstanding scenarios
remain in the [acceptance ledger](PARITY_ACCEPTANCE.md).
