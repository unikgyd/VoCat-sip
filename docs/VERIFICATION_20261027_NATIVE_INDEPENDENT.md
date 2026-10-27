# Independent native deployment checks — 2026-10-27

The checked `f5eea23` candidate completed two additional NAS deployment checks
while the original CT data owner retained its unresolved cleanup capability.
These checks did not reboot a modem, change native IMS settings, send carrier
traffic or force a claim. The [public record](verification-20261027-native-independent.json)
contains the artifact/source hashes, exact scope and restoration assertions.

They extend the [native owner/DNS run](VERIFICATION_20261027_NATIVE_OWNER_DNS.md).
They do not complete its failed IPv6 main-process recovery case.

## Proxy unit under actual systemd protection

After DITO restoration, the host already had a serving QMI proxy. The initial
hardware-access experiment refused it before installing its unit or binary.
The checked `vocat qmi-proxy` command was subsequently invoked against that real
serving proxy and returned exit 1 without adopting or stopping it. Original
proxy PIDs, start times and cgroups remained unchanged.

The new proxy unit was then executed in a separate real network namespace.
Only its unit name, checked executable path and `PrivateNetwork=true` differed
from the frozen source. The actual service retained `NoNewPrivileges`, private
temporary storage, strict system protection, protected kernel/control groups,
the original capability bound and AF_UNIX/AF_NETLINK restriction.

The real libqmi proxy passed its `ExecStartPost` socket check. A disposable
systemd client joined that namespace and connected without sending a QMI request.
Its actual main PID was SIGKILLed; systemd recorded killed/status 9. The proxy's
PID, invocation, start time and namespace remained unchanged with zero automatic
restarts, and another readiness check succeeded afterward. This exercises real
native unit protection and cgroup isolation, without claiming modem-data access.

After confirming that no client connection remained, only the owned temporary
proxy service was stopped. It stopped successfully. Its unit and copied binary
were archived outside the installation paths; neither remained installed.
Cold recovery of a real VoCat main/data session still requires its own live case.

## Fresh administrator and server bootstrap

A fresh database was created through the checked binary's real
`bootstrap-admin` command. A separate native service then ran the same binary
against that database, with real modems, host D-Bus and the host runtime lock
hidden by explicit OS isolation. Its network and instance lock were independent.
A pre-exec guard verified isolation before the application started.

This was a manually staged, isolated native service. Its working/database/env
and runtime paths were adapted, hardware/sysfs mounts were hidden, and Type=exec
was used to attest isolation after namespace setup. Automatic retries were
disabled after the retained harness failures. It was not an execution of the
standard installer or public GitHub release delivery.

| Real check | Result |
| --- | --- |
| Bootstrap a nonexistent database | `created`; one administrator and valid SQLite database |
| Anonymous device access / wrong password | HTTP 401 |
| Original administrator login and session lookup | HTTP 200 |
| Actual hardware inventory in the isolated service | Empty |
| Embedded HTML and index JavaScript | HTTP 200; hashes matched the frozen frontend |
| Logout without CSRF | HTTP 403; original session remained valid |
| Orderly stop, repeated bootstrap with another password | Exit 0; `exists`; original administrator unchanged |
| Fresh service invocation using the same database | Original session and password still worked; replacement password rejected |
| Valid-CSRF logout and subsequent session lookup | HTTP 200, then HTTP 401 |
| Final stop and database verification | Exit 0; integrity `ok`; original administrator ID/hash/timestamps retained |

No browser or EULA acceptance is claimed by these API/bootstrap checks. The
fresh database had no modem rows and the service could not reach real hardware
or host management interfaces. Its complete temporary installation and runtime
paths were removed after both orderly runs.

## Retained harness findings

The first coarse sysfs mask omitted systemd's cgroup and memory-pressure mount
targets, causing exit 226 before application execution. The mask was narrowed
to the modem-discovery directories. An early Type=simple observer had also seen
the pre-exec namespace stage; Type=exec and the pre-exec guard removed that
observation ambiguity.

A prototype fixture's writable `/run` bind exposed the original host instance
lock. The actual application refused startup before device discovery. The bind
was removed; the final guard and the application's private lock inode then
proved the intended isolation. A not-loaded `reset-failed` result after daemon
reload was handled from the verified stopped fixture state. None of these
failures was counted as a successful service run.

## Current host and remaining work

The original CT owner PID and claim remained unchanged. Fresh read-only
attestation confirmed the original CT SIM/equipment, unchanged profiles and
native IMS `1,1`. DITO remained registered with RF on. The original database,
lock, containers and ModemManager were preserved, and the independent experiments'
installation paths and units were removed.

The earlier failed CT phase remains incomplete: its original owner still retains
the claim and flock after loss of its original proxy transport. The separately
requested recovery reboot is still awaiting operator authorization. The
dedicated proxy's cold carrier recovery, standard empty-machine installation,
public release delivery and the remaining [hardware/carrier acceptance](PARITY_ACCEPTANCE.md)
are open.
