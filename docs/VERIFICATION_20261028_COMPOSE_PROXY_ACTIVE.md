# Shipping Compose: active ordinary-data proxy loss

2026-10-28. Three real CT data cases passed with the shipping Compose restart,
health and dependency settings, using the existing opt-in original-owner recovery.
IPv4, IPv6 and dual stack recovered after an actual proxy SIGKILL while the
original main and data owner survived. The fault cases used the matching patched
ModemManager. Returning to the distribution manager produced one SIGSEGV and
automatic restart; that dependency failure remains recorded. The original
environment was subsequently restored after a separate normal service restart
and a 120-second stability observation. This is qualified scenario acceptance;
the complete acceptance goal remains open.

The [sanitized evidence](verification-20261028-compose-proxy-active.json) records
the three cases, default-supervision contract, source/helper hashes, retained
failures and final restoration. Subscriber data, configuration secrets, public
addresses, syscall traces and the crash core remain in private NAS storage.

## Unchanged candidate and actual deployment

Tested main revision: `4801f900ccffe0dc35ab8b021bf1f99b046e35a1`. All 1,818 selected
product inputs, including all 868 frontend inputs, matched the preceding frozen
candidate. The amd64 binary SHA-256 was
`18c88714414563bf732c02f9d5cfc15d9bd18bf54707c810c00bceb63e42ee47`.
The [preceding software checks](VERIFICATION_20261028_ASTERISK_SESSION.md) passed
2,131 Go cases, 320 selected race executions, three kernel cases, vet and four
Linux builds. They were not rerun for this unchanged runtime. The 48 environment
skips remain skips.

Docker Compose executed hash-checked copies of `docker-compose.yml` and
`compose.data-owner.yaml`. The private override selected an existing runtime
image, the same checked read-only binary in all three services, an independent
database/SIM admission file, private names, API address and socket. Default
`unless-stopped` policies, host network/PID namespaces, shared locks and state,
device/sysfs permissions, stop grace and proxy health/dependency gates remained
in effect. The main image's default entrypoint ran. No image was pulled or built.

`VOCAT_DATA_OWNER_RECOVERY_PORTS` was explicitly configured only in the owner,
for the original QDC507 physical discovery key and its secondary `ttyUSB3`.
Recovery remained data-only: Host IMS, VoWiFi, SIP, scheduled business and
external notification dispatch were disabled in the private fixture. Native
modem IMS stayed `1,1`. The original equipment, ICCID, IMSI and physical USB
generation were verified before handing over hardware; production recovery
repeated its own identity, policy, RF and native-call guards at submission.
The Philippines card received no call or SMS submission.

The matching temporary ModemManager build had SHA-256
`6b319c980e33256162ba68a4505807255eebe0f81dadde84f5b75979f61e41ac`.
The distribution packages were ModemManager `1.25.95-1ubuntu1` and libqmi
`1.38.0-1`. The existing [dependency patch and deployment notes](HOST_IMS_DEPLOYMENT.md)
remain relevant; these cases do not accept the unpatched manager's reliability.

## Real carrier results

Each case first explicitly enabled the requested family and completed a real
public-address query. The controller then checked the exact original proxy
container, PID/start time and executable identity, opened its process descriptor,
rechecked it, and sent one SIGKILL. Docker recorded exit 137 and one automatic
proxy restart. No manual modem restart, main/owner restart or new enable request
occurred within a fault/recovery window.

| Requested data family | Automatic recovery including public query | Observed result after recovery |
| --- | --- | --- |
| IPv4 (`IP`) | 56.954 s | HTTP 200, IPv4 address; global IPv4 interface address |
| IPv6 (`IPV6`) | 56.383 s | HTTP 200, IPv6 address; global IPv6 interface address |
| Dual stack (`IPV4V6`) | 39.751 s | HTTP 200, IPv6 address; both global IPv4 and IPv6 interface addresses |

The original main and owner kept their process/container identities across all
three cases with zero container restarts. The same proxy container automatically
restarted once per case, three times in total, and returned healthy. No supervisor
dependency cascade terminated either client in these Compose runs.

For each original claim, a 20 ms observer captured the original record, its
durable one-shot recovery intent, an empty fence after cleanup, and a fresh
record on a new USB generation. Original equipment, physical-device, SIM,
interface, owner PID and boot bindings matched. At the empty-fence boundary,
original policy rules were restored, global data addresses were absent and the
private DNS file was gone. The new record did not adopt the old token or WDS
capability. Separate captures confirmed one complete 12-byte CFUN write on the
configured secondary port and one successful Raw-IP `Y` write per case.

The controller normally disabled data after each accepted case and verified
that its claim, routes, global addresses and private DNS were released before
starting the next family. These explicit requests separate the three cases;
they did not trigger recovery of a preceding fault. Dual-stack evidence includes
both configured address families and a successful IPv6 query. A new simultaneous
transfer capture for both families was not taken in this phase.

## Retained failures and restoration

The first IPv4 preparation used the unsupported card-policy value `IPV4` instead
of `IP`. Validation refused it before data enable, claim creation or any fault
signal. Its raw HTTP response was not retained. The harness input was corrected,
the initial version archived, and all nine controller/helper inputs frozen
again before the three actual data cases. Project runtime code was unchanged.

All test services stopped and their data state was released. During return to
the original distribution ModemManager, the service exited with SIGSEGV and
systemd restarted it once. The strict first restoration audit failed its
zero-automatic-restart condition. The failed audit, journal and 54,153,216-byte
core were preserved. Core SHA-256:
`b23b95000d1a898c6ddbd168708a7ab4efce11e101c175fa8845130d5066c897`.
No fresh core stack was obtained, so the cause of this specific crash is not
established by the earlier known netlink lifetime defect or nonfatal GSM decode
messages. Successful VoCat cleanup does not turn the dependency failure green.

With all claims and owned inhibitors already released, a separately recorded
normal restart returned the original distribution executable. No `reset-failed`,
modem reset or journal deletion was used. Its PID remained stable with zero
further automatic restarts over the 120-second observation; final reconciliation
completed at 121.555 seconds. The final zero counter belongs to this later
startup. The initial SIGSEGV and restart count of one remain explicit in the
record. This restored the environment without fixing the dependency bug.

Final checks confirmed both original equipment/SIM pairs registered with RF on,
native IMS and PDP definitions preserved, the distribution executable restored,
and the temporary override/executable removed. Original root database hash,
shared lock inode, five running container IDs and PC/SC service/socket state
matched. CT host state matched except its necessarily recreated interface index;
the Philippines host snapshot matched exactly. No claim, XFRM state/policy or
private DNS file remained. Exact labelled Compose containers were removed;
no project network or volume remained. Normal exit was checked before each
guarded removal; main/owner also have full final container inspections.

## Remaining acceptance

The original owner survived every injected fault. Active-claim owner death still
needs a confirmed orphan/resource recovery path before destructive testing;
current persistent records deny fresh admission when that capability is lost.
Unconfigured recovery, native systemd dependency cascades, host/power loss,
physical hotplug and other task/provider scenarios remain in
[the acceptance ledger](PARITY_ACCEPTANCE.md). Distribution-manager callback
reliability also remains open. This phase initiated zero carrier calls and SMS,
and used no ADB voice or maintenance operation. Physical browser audio stays
deferred at the user's request.
