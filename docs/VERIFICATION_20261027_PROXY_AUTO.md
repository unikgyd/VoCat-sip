# Original-owner QMI transport recovery — 2026-10-27

The independent ordinary-data owner now has an opt-in recovery path for loss of
its original QMI transport. It remains disabled without an explicit physical
device/secondary AT mapping. The [deployment and admission contract](DATA_OWNER_RECOVERY.md)
describes the guards, one-shot durable intent and retained cleanup capability.
The earlier [containment/manual-maintenance case](VERIFICATION_20261027_PROXY_LOSS.md)
remains a separate historical result.
The [machine-readable record](verification-20261027-proxy-auto.json) includes
the frozen builds, retained failures, actual recovery and final restoration.

## Implementation

The original owner recognizes typed permanent socket failures, verifies the
original equipment/SIM and current data-only policy through a separately
configured secondary AT port, and durably records its one allowed attempt before
submitting CFUN. Identity, USB generation, policy, RF and native voice checks
repeat at the write boundary. Unknown reads, uncertain persistence and missing
responses retain the fence; command acknowledgment cannot release it.

After a successful data start, the owner independently observes its retained
transport every five seconds. The watch outlives the completed main-process RPC.
Stop/close cancels and joins it before cleanup. This avoids depending on a new
Set request whose admission can already be blocked by the main process's failed
SIM refresh. A replacement-generation Set first requires the original peer's
cleanup acknowledgment and absence of its durable claim.

Temporary DMS identity clients are now released on both successful and failed
reads, and a release failure is returned. Closing the underlying QMI transport
does not itself release those service CIDs. A production-client wire regression
uses a one-slot synthetic DMS pool across 24 alternating successful/failed reads
and separately checks release-error propagation.

## Frozen software candidate

The final source manifest SHA-256 is
`09231c2544150e1c896e88a0ed760bccbda57380df3ee03e4c4e40649e257f73`.
Its archive contains 1,917 inputs, based on main revision
`c96463c2d0ed879a37188a9a7c461096b53a0606` plus this implementation. All 1,784
selected Go/frontend/script/deployment/module inputs match the frozen manifest.
The Linux amd64 artifact is
`cf7c11fedba7aa58435180fda225fd44692d5294a22956f55e14171deccfdc20`.

Each completed implementation was frozen before its combined checks. Actual
software/hardware findings required subsequent candidates; failed candidates
were retained. There were no targeted test reruns between implementation edits.

| Final check | Result |
| --- | --- |
| Full Linux Go suite, CGO disabled | 2,115 passed, 0 failed, 48 skipped |
| Isolated ordinary-data kernel checks | 3 passed, 0 skipped |
| Vet | Passed |
| Selected recovery/identity race checks | 85 passed executions over five iterations, 0 failed/skipped |
| Static Linux builds | amd64, arm64, 386 and armv7 passed |
| Frontend | All 868 inputs unchanged; previous 82 cases/build referenced, not rerun |

The final run used Go 1.27.3, pinned offline modules and private mount/network
namespaces with isolated sysfs and `/run`. The count includes 17 new top-level
cases and two formerly skipped AMR cases which now execute with the previously
installed narrowband runtime. This does not establish a new carrier audio case.

The first combined candidate failed to build `cmd/vocat` because a test used a
nonexistent Host IMS constant. The next full run exposed an unbound legacy
VoWiFi-policy admission gap. Both were repaired before another complete batch.
The following candidate passed 2,112 Go/70 race cases; the DMS fix candidate
passed 2,113 Go/75 race cases. Those successful software runs did not turn their
failed hardware scenarios into passes.

## Retained hardware failures

The first actual proxy SIGKILL used the 2,112-case candidate. The original claim's
durable recovery intent, one secondary-port CFUN write, USB generation change and
old-claim cleanup were observed. Automatic reconnection did not complete within
the five-minute window. The private fixture lacked the shipping writable
`/sys/devices` bind: the new interface had Ethernet framing and the owner's
Raw-IP path was not writable. Late file observers did not capture the actual
denied write; the mount/access findings are retained separately from syscall
evidence. The shipping Compose files already contained the required mount.

Repeated attempts also exposed the missing DMS release in production source.
ModemManager reported `ClientIdsExhausted` and could not initialize the modem.
After both clients stopped with no data/Host IMS claims, host framing was restored
and one separately recorded CFUN maintenance command was acknowledged without an
observed USB generation change. It was not replayed. One exclusive maintenance
CTL sync, with the manager/proxy stopped and no other control opener, restored
the DMS pool and original registration. This was operator restoration, not the
shipping recovery path or adoption of a lost CID.

The second actual fault used the 2,113-case candidate and correct writable mount.
It retained the exact original claim for the five-minute observation, with no
automatic recovery intent or CFUN write. Main-process live-SIM observation
failed before further Set admission; a surviving owner needed its own watch.
That finding led to the final implementation above.

Controlled restoration of the second case first disabled data and quiesced the
main while retaining the original owner/lock. One secondary-port CFUN was
acknowledged but did not produce USB generation change or old-claim release in
the bounded window. A separate complete module reboot then used an exact USB
port/ADB transport, after fresh original-SIM verification. The original owner
observed generation loss and released its own claim. ADB was used solely for
module maintenance, with no voice/audio bridge. Its private server was stopped.
The main was manually restarted with data disabled, then the entire fixture was
restored. No claim was forced and no packet handle was adopted.

Both failed phases returned the original equipment/SIM pairs registered, native
IMS at 1, the stock manager with zero automatic restarts, original database hash,
shared-lock inode, running-container IDs and host configuration. The CT netdev
index changed with enumeration; the Philippines host snapshot matched exactly.

## Final hardware run

The final candidate passed one actual proxy-loss automatic recovery. The exact
proxy process was killed through a PID descriptor; Docker recorded exit 137 and
one automatic proxy restart. The main and original owner kept their original
PIDs/start ticks, with zero container restarts. Both clients had the shipping
writable `/sys/devices` bind and used the same checked binary read-only.

The observed claim sequence was the original claim, its durable recovery intent,
no claim after original cleanup, then a fresh claim on the same equipment/SIM and
new USB generation. Exactly one secondary-port CFUN write was observed. Original
policy rules/DNS and global data addresses were cleared before the new claim;
the recovered interface's
successful Raw-IP write was captured with no read-only-file errors. Saved data
intent stayed enabled. The real public query returned HTTP 200 using IPv6 after
61.813 seconds, including the query. No manual modem reboot, main/owner restart
or new data-enable request occurred after the injected fault.

After acceptance, one explicit disable completed with no claim, global data
address, owned routing or private DNS left. Final host/service restoration is
recorded in the accompanying machine-readable audit.
All three owned containers exited normally. Both original SIMs registered with
RF on; the QDC507 native IMS setting stayed at 1. Its IMS setting and PDP
definitions matched the snapshot taken before all three phases. Stock
ModemManager returned with zero automatic
restarts; all owned overrides, executables and inhibitors were removed. Original
database hash, shared-lock inode and running-container IDs were preserved, CT
host state matched apart from the recreated interface index, and Philippines
host state matched exactly. After clients stopped, the pre-test link flag was
restored as needed; no Raw-IP repair or firmware action was needed after this
successful recovery. No data/Host IMS claim, XFRM state/policy or private DNS
remained. Owned ADB maintenance servers from the earlier phases were absent.

This was a private host-network/PID Docker fixture with main/owner restarts
disabled and proxy restart on failure. It did not execute unmodified shipping
Compose supervision or systemd dependency cascades. The earlier CFUN/no-USB-change
maintenance findings remain: this single successful automatic run does not
guarantee that every acknowledged reset produces generation loss.

## Scope

This is ordinary QMI data under a data-only module policy. No carrier call or SMS
was submitted and the Philippines SIM sent no SMS or calls. Pure Host IMS voice
acceptance remains in its separate reports. No packages were added in this phase;
existing private bundled ADB tools were reused only for the failed-case maintenance.
Raw subscriber identities, credentials, data claims, network addresses and logs
remain outside Git.

A CFUN acknowledgment alone does not establish USB disappearance or packet
cleanup. A consumed intent or uncertain cleanup can still require maintenance.
The original independent owner must survive; owner death, cold orphan recovery,
machine/power loss and default Compose/systemd dependency cascades remain separate.
Browser audio, remote IVR recognition, further call-ending/fault scenarios and
additional hardware/profile/provider combinations keep their existing open entries.
