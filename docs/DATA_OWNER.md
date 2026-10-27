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

Install `deploy/vocat-data-owner.service` as
`/etc/systemd/system/vocat-data-owner.service` and `deploy/vocat-data-owner.conf`
as `/etc/systemd/system/vocat.service.d/data-owner.conf`. Reload systemd, enable
and start `vocat-data-owner`, then restart `vocat` normally so its existing data
capability drains before switching backends. The drop-in enables
`VOCAT_DATA_OWNER_SOCKET=/run/vocat/data-owner.sock` for the main service. The
owner unit sets the matching socket path. The existing installer does not yet
enable this optional two-service layout automatically.

For upgrades, drain and stop the main service first, then stop the owner, replace
the shared binary, and start the owner before the main service. Restarting only
the main service does not reload the independently running owner's executable.

## Docker deployment

Build the candidate and start the provided override together with the base file:

```sh
docker compose -f docker-compose.yml -f compose.data-owner.yaml up -d --build
```

The override adds a separate owner container and shares the database, namespace
and lock mounts. Apply any custom database or authorized-SIM environment/file
mounts to both services. Restart the main container independently when exercising
main-process recovery; recreating both containers terminates the surviving owner.

## Limits

The persistent fence has no expiry or force takeover. An unknown Start that
returned no usable handle can remain pending even in the original owner. If
the owner itself dies, its unresolved journal still denies new data/Host IMS/RF
admission; this implementation does not reconstruct its lost WDS capability.
Host IMS has its own [confirmed active-process case](VERIFICATION_20261027_HOSTIMS_PROCESS.md);
machine/power loss remains separate acceptance. Existing IPv4 host-resource
ownership limitations are not changed by moving the original manager into a
surviving process.

See [the data fence](DATA_OWNERSHIP_FENCE.md) and
[main-process crash acceptance](VERIFICATION_20261027_DATA_OWNER.md) for the
executed checks, retained initial failure and hardware boundaries.
