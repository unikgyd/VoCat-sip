# Native systemd and public-IP acceptance — 2026-10-27

This continues the [IPv6/dual-stack run](VERIFICATION_20261027_IPV6.md), from
source baseline `890a8adc966d24a8c1e6d2f39dd2cee47b76e974`. The
[machine-readable record](verification-20261027-native.json) binds the tested
runtime sources, build hashes, outcomes and restoration. Documentation was
updated after the frozen software run. The overall hardware goal remains open.

## Native installation and failed upgrade

The full production installer ran against real NAS systemd, with its actual
hardening and default installation paths. Those paths were absent beforehand.
A private copy of the existing database supplied the original administrator;
both modem access auto-start settings and cellular data started disabled.
Notification channels and scheduled tasks were inactive. The exact authorized
test-SIM policy was supplied privately; no subscriber identity was committed.

Release downloads used a temporary transport fixture serving only the exact
binary/checksum URLs for the checked build. Health requests used real curl and
the real service. This establishes the installer lifecycle with controlled
release bytes; it does not establish Internet GitHub release delivery or
first-administrator creation from an empty database.

| Scenario | Observed result |
| --- | --- |
| Incorrect release checksum | Installer rejected it before installing a binary or writing the unit. |
| Valid release | Actual service became healthy; original administrator login, both original modem refreshes and existing CTNET APN discovery passed. |
| Second server with a different database | Host-wide lock rejected it before opening that database or accessing modem resources. |
| Failed upgrade with IPv6 data enabled | A controlled executable passed version/media/bootstrap diagnostics, then exited after three seconds. Installer returned failure, restored the checked previous binary and confirmed HTTP health; its backup remained. |
| Saved data intent after rollback | Original IPv6 data reconnected automatically, and a new public-IP query succeeded without another data-start submission. |
| Runtime directory preservation | Original shared-lock inode/content and a private test marker survived service restarts and final stop. |

The first rollback observer stopped at the startup state `no process-owned QMI
data session`. A subsequent read confirmed the existing intent had reconnected
under the restored service, with no automatic service restarts or resubmitted
data request. That initial observer failure is retained rather than counted as
a passing observation.

The installer now requires two consecutive successful HTTP health responses
from the same service process before removing its previous binary backup. A
failed upgrade restores the binary atomically, checks its health and retains
the backup. Generated and checked-in units preserve `/run/vocat`; the checked-in
unit also uses the installer's `/etc/vocat/env` path and permits the existing
updater to write `/opt/vocat/bin`. Bash syntax, five health-address parsing cases
and unit verification while the executable was installed passed. After cleanup,
another unit verification correctly reported that the archived executable was
absent; that post-cleanup check is not counted as a successful installation.

## IPv6 public-IP failure and repair

The initial native build established real IPv6 data but returned HTTP 502 for
public-IP lookup after its deadline. Through the original carrier DNS,
`ipinfo.io` had one A record and no AAAA record; `v6.ipinfo.io` had an AAAA record
and no A record. A control hostname had both. The separate legacy API endpoints
are documented by [IPinfo](https://ipinfo.io/developers/ipinfo-api).

The final implementation queries both official legacy endpoints concurrently
inside the same revocable cellular lease and existing marked/interface-bound
transport. The first valid response cancels the other query; both workers exit
before the lease is returned. Invalid responses cannot win. Tests cover either
working family, cancellation/join, an invalid response preceding the other
family's success and parent cancellation.

On the original QDC507/authorized China Telecom SIM and its existing CTNET APN,
the final native build returned HTTP 200 with a valid public IPv6 address and
country code, repeated successfully after failed-upgrade rollback. A separate
real IPv4 connection returned IPv4. Addresses remain private. Data-off cleared
the cache in both families and restored original policy rules, main defaults,
IPv6 settings, MTU and link state; owned global addresses and private DNS files
were absent afterward. No carrier call, radio reset or mobile SMS was submitted.

## Final frozen software and dependencies

The final runtime sources passed **2,062 Go tests**, zero failures and
48 opt-in/environment skips, vet, **20 selected race-test executions** and
Linux amd64/arm64 builds. General checks used `CGO_ENABLED=0`; race checks used
`CGO_ENABLED=1`, in isolated network/mount namespaces. Earlier frontend and
kernel suites were not repeated or added to these counts.

| Artifact | SHA-256 |
| --- | --- |
| Installed/tested Linux amd64 | `4fea12ecb7153f68c70929878593fc2bb71c7ba87bd48577a5eec383fcb66a8f` |
| Built Linux arm64 | `605eb18d254d0c4624e59e25ebd210d4b71566fd8a0d4c5d024fb3af8f002c03` |

The native host temporarily installed `libccid 1.7.1-1`, `pcscd 2.4.1-1` and
`libopencore-amrnb0 0.1.6-1build2`. The native service API reported AMR-NB ready,
alongside PCMA/PCMU; AMR-WB was not built. No USB CCID reader was present, so
PC/SC service installation does not establish physical reader acceptance.

As in the previous run, the matching ModemManager 1.25.95 build used the
[netlink completion patch](patches/modemmanager-1.25.95-netlink-completion.patch).
It retained the same process with zero restart delta through data and inhibitor
release. The original distribution executable remained untouched and was
restored afterward; its earlier failure remains a separate dependency limit.

## Restoration and remaining work

The application stopped successfully with exit code 0. Only owned native
installation/configuration paths were archived and removed; the shared runtime
directory and original lock remained. The original database was byte-identical,
original running Docker container IDs were preserved, and the previous test
container remained stopped. The temporary manager binary/drop-in were removed;
both original modem/SIM pairs registered with RF on under the original manager.
Host IMS claims, task recovery journals and XFRM states/policies were zero.
Exactly the three newly installed dependency packages were removed, restoring
the original package names, versions/states and absent PC/SC service.

Answered/incoming carrier voice, advanced carrier media, mobile SMS, physical
hotplug, abrupt crash recovery, successful task business execution, browser work
past the first-use agreement, external notification delivery and disposable eSIM
provisioning/download/delete retain their separate acceptance. This run tested
orderly service restart and upgrade rollback. VoWiFi carrier calls remain outside
the requested scope. See the [acceptance ledger](PARITY_ACCEPTANCE.md).
