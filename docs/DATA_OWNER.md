# Surviving ordinary-data owner

The optional Linux `vocat data-owner` process owns the original QMI clients,
packet handles, SIM/USB binding, host configuration and durable data fence.
`vocat serve` sends data operations through a private Unix packet socket. The
owner runs independently of the HTTP service, so terminating the HTTP process
does not discard its packet cleanup capability. AT, voice, SMS and eSIM remain
with the main device manager.

AT packet-data owners also remain local and use their original stop path.
An unreleased local AT owner cannot be replaced by a delegated QMI start; an
AT activation on a QMI-capable modem first joins any admitted delegated owner.
AT-only devices do not require an available QMI helper for ordinary RF/data
cleanup. These dispatch boundaries have separate software regression cases.

The two processes must use the same candidate binary, database directory, PID
and network namespaces, and shared `/run/vocat` and `/run/lock`. The socket lives
in an owner-only directory, has mode 0600 and checks peer credentials. Requests
carry the original physical generation and SIM constraints; the owner rechecks
them through its own actual QMI transport before starting data.

Each admitted peer transfers a duplicate of the main instance's existing flock
descriptor. The owner retains it until cleanup is acknowledged. Main-process
death therefore leaves the instance lock held while the owner cancels and joins
the original operation, clears its host configuration and stops its original
packet call. A new main process cannot run its startup RF/data work before that
barrier is released. Supervisor retries can then restore the saved data intent.
No CID/handle is transferred to a replacement process or adopted from disk.

Database attestation uses a parent-directory descriptor and stat metadata.
Neither side opens/closes an extra database-file descriptor outside SQLite:
doing that can cancel SQLite's POSIX file locks, as documented in
[SQLite's locking notes](https://www.sqlite.org/howtocorrupt.html#_posix_advisory_locks_canceled_by_a_separate_thread_doing_close_).
A separate-process WAL regression checks that the attestation preserves live
SQLite locking and visibility.

## Native deployment

Use the same binary and `/etc/vocat/env` for both services, including the same
private authorized-SIM file when one is configured. `VOCAT_DATABASE_PATH` must
be a literal absolute filesystem path. The standard environment uses
`/opt/vocat/data/vocat.db`.

Install `deploy/vocat-qmi-proxy.service` as
`/etc/systemd/system/vocat-qmi-proxy.service`, `deploy/vocat-data-owner.service` as
`/etc/systemd/system/vocat-data-owner.service` and `deploy/vocat-data-owner.conf`
as `/etc/systemd/system/vocat.service.d/data-owner.conf`. Reload systemd, enable
and start `vocat-data-owner`, then restart `vocat` normally so its existing data
capability drains before switching backends. The drop-in enables
`VOCAT_DATA_OWNER_SOCKET=/run/vocat/data-owner.sock` for the main service. The
owner unit sets the matching socket path. Its shared QMI transport runs in the
separate proxy unit, ordered before both clients. `vocat qmi-proxy` replaces its
process with the installed libqmi proxy using `--no-exit`; this preserves its
systemd cgroup and avoids an idle exit. The existing installer does not yet
enable this optional layout automatically.

The proxy's `ExecStartPost` checks its actual shared socket before either client
starts. The same check gates both Compose clients on a healthy proxy container.

An automatically launched proxy can belong to the HTTP service's cgroup.
Systemd's default control-group cleanup then kills that proxy when the main
process dies, disconnecting even a surviving owner's original WDS transport.
A detached process group does not prevent this. The dedicated unit prevents
that lifecycle coupling. The launcher refuses an already serving proxy rather
than adopting or stopping it. Drain existing clients and arrange supervision of
their original proxy before changing layouts; another application's proxy must
not be killed to enable VoCat.

For upgrades, drain and stop the main service first, then stop the owner, replace
the shared binary, and start the proxy, then the owner, then the main service. Restarting only
the main service does not reload the independently running owner's executable.

## Docker deployment

Build the candidate and start the provided override together with the base file:

```sh
docker compose -f docker-compose.yml -f compose.data-owner.yaml up -d --build
```

The override adds separate owner and proxy containers and shares the database,
namespace and lock mounts between both clients. Apply any custom database or authorized-SIM environment/file
mounts to both services. Restart the main container independently when exercising
main-process recovery; recreating both containers terminates the surviving owner.

## Limits

An opt-in [original-owner transport recovery](DATA_OWNER_RECOVERY.md) adds one
durable restart intent through an explicitly configured secondary AT port.
It requires the original SIM, a data-only policy and confirmed permanent socket
loss. Submission never clears the fence; the original owner must still confirm
old-generation cleanup. Without that configuration the behavior below remains.

The persistent fence has no expiry or force takeover. An unknown Start that
returned no usable handle can remain pending even in the original owner. If
the owner itself dies, its unresolved journal still denies new data/Host IMS/RF
admission; this implementation does not reconstruct its lost WDS capability.
Host IMS has its own [confirmed active-process case](VERIFICATION_20261027_HOSTIMS_PROCESS.md);
machine/power loss remains separate acceptance. Existing IPv4 host-resource
ownership limitations are not changed by moving the original manager into a
surviving process.

Losing the QMI proxy also loses the original transport. The journal and shared
instance lock stay retained when original cleanup cannot be confirmed. Starting
a fresh proxy does not restore that capability. Such a failure needs controlled
operator recovery and cannot be counted as automatic main-process recovery.

The [actual proxy-loss follow-up](VERIFICATION_20261027_PROXY_LOSS.md) exercised
that boundary with the original owner surviving. A real proxy SIGKILL and
automatic proxy restart retained the exact old claim; unconfirmed data queries
were rejected. One original-modem reboot allowed that same owner to acknowledge
old-generation loss and release its own claim/instance lock. A subsequent
explicit data enable and real public query passed. No journal was forced or
lost capability adopted. This private Docker case used deliberate main/owner
restart policies; owner death and unchanged shipping supervision fault behavior
remain separate. Those observations did not establish automatic proxy recovery.
The later [opt-in recovery follow-up](VERIFICATION_20261027_PROXY_AUTO.md) adds
the original owner's independent transport watch, one durable reset attempt and
DMS identity-client cleanup. One actual proxy SIGKILL recovered automatically
with the original main/owner retained and a real IPv6 public query. Retained
failures, reset acknowledgment without generation change and unchanged shipping
supervision boundaries are recorded separately there.

See [the data fence](DATA_OWNERSHIP_FENCE.md) and
[main-process crash acceptance](VERIFICATION_20261027_DATA_OWNER.md) for the
executed checks, retained initial failure and hardware boundaries.

The [independent native follow-up](VERIFICATION_20261027_NATIVE_INDEPENDENT.md)
also executed the new proxy unit's actual protection/readiness settings and a
disposable systemd client SIGKILL in a private network namespace. A real already
serving host proxy was refused without replacement. Those checks do not accept
the cold carrier recovery scenario. The subsequent
[real three-unit run](VERIFICATION_20261027_NATIVE_COLD.md) passed main-process
SIGKILL recovery for IPv4, IPv6 and dual stack. It also records controlled
restoration of the earlier failed phase and the separate maintenance reboot
needed for initial IPv6 reachability. Neither result establishes owner/proxy
process-loss recovery.

The later [shipping Compose active-proxy run](VERIFICATION_20261028_COMPOSE_PROXY_ACTIVE.md)
exercised unchanged default restart/health/dependency settings with an explicit
secondary-port recovery mapping. Actual IPv4, IPv6 and dual-stack proxy SIGKILLs
recovered with the original main and owner retained. The fault cases used matching
patched ModemManager; one distribution-manager restoration crash is retained,
with the original environment subsequently restored. That earlier run did not
accept owner death, unconfigured recovery or native systemd dependency cascades.

The later [active-owner run](VERIFICATION_20261028_COMPOSE_OWNER_ACTIVE.md) adds
recovery after a process holding an acknowledged active claim dies. Version 2
stores process/namespace identity and a committed host receipt; a replacement
uses the same opt-in secondary port, one durable reset intent and exact original
USB/netdev/rule/DNS cleanup proofs. No WDS capability is adopted. Shipping Compose
IPv4/IPv6/dual-stack faults passed with the original main/proxy retained. Its
IPv6 baseline maintenance, syscall-observation limit and patched-manager scope
remain explicit. Legacy/unready records, host boot/power loss and native
supervision cascades still require separate handling and acceptance.
