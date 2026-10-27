# Opt-in recovery of a lost ordinary-data QMI transport

The independent data owner can optionally restart its original modem after a
confirmed permanent failure of its own QMI socket. This requires an explicitly
configured secondary AT port and a current, unambiguous data-only policy for
the original SIM. It is disabled by default. This is a physical modem restart;
native IMS and other connections on that modem are interrupted temporarily.
No SMS or voice call is placed by recovery.

Set `VOCAT_DATA_OWNER_RECOVERY_PORTS` in the **data-owner** process environment
to a JSON object such as:

```json
{"your-discovery-key":"/dev/serial/by-id/your-secondary-AT-port"}
```

Use `discovery_key` from the authenticated `/api/devices/discovered` response,
not the configured application line ID. The same response lists `at_ports` and
the selected primary `at_port`. Reserve and configure a different AT-capable
port of that physical modem. A stable `/dev/serial/by-id/` path is preferable;
the actual resolved port, USB parent and enumeration generation are checked
again at the write boundary. There is no wildcard, alternate-card selection or
implicit primary-port fallback. Invalid/duplicate configuration prevents the
owner from starting. An empty or absent mapping leaves existing behavior intact.

The owner and main still need the same checked binary, database and shared
lock/namespace layout described in [DATA_OWNER](DATA_OWNER.md). For systemd,
the existing environment file can carry this setting; only `data-owner` uses it.
For Docker, provide the setting to the owner container and retain the documented
device, sysfs and lock mounts. No extra service or external shell command is used.

Keep the shipping writable `/sys/devices` bind for both Docker clients, beneath
the read-only `/sys` bind. Native deployment likewise needs the unit's
`ReadWritePaths=/sys/devices`. After USB enumeration, a new netdev can start in
Ethernet framing; its owner must restore QMI Raw-IP through sysfs. A proxy health
check and a successful connection on an already configured netdev do not verify
this permission. Identity reads release their temporary DMS client even when
reading fails; closing the transport alone does not release a service CID.

## Admission and one-shot intent

Recovery requires all of the following:

- The original live owner still holds its durable claim and known successful
  WDS handle. Unknown initial Start results and cold orphan records cannot reset.
- The original socket returned a permanent EOF/closed/reset/broken-pipe error.
  Protocol errors, generic error strings, timeouts and a disconnected packet call
  alone are insufficient. Every still-attempted family in a dual-stack owner must
  have lost its transport.
- The saved original SIM still has exactly one matching data-enabled module
  policy. Changed-card, Host IMS, Wi-Fi-enabled, ambiguous and unbound protected
  configurations deny the restart. This policy is re-read before submission.
- The configured secondary port belongs to the original current USB instance.
  Fresh card/subscriber reads bracket identity checks on that port; equipment
  identity must also match the original claim.
- Live RF mode is online and a successfully parsed CLCC contains no voice call.
  Non-voice data records do not count as a voice call. Unknown responses deny it.

The original claim stores `RecoveryAttempted` using an exact-record SQLite CAS
before `AT+CFUN=1,1`. The identity, physical generation, policy, RF and native
call checks repeat after that durable intent. Each original claim permits at
most one attempt, including a rejected command, missing response or cancellation.
An uncertain persistence result never authorizes a reset. An unknown AT response
retains that original serial dialogue rather than reopening it for another
attempt. The owner never changes the native IMS setting.

Submitting or acknowledging CFUN does not release the fence. The original owner
must observe disappearance/change of its original USB generation, clear its own
host resources, close the retained recovery serial client and acknowledge its
own claim deletion. Only then can the existing saved data intent start a fresh
session through the replacement proxy. No old CID or packet handle is adopted.

After a successful start, the opt-in original owner checks its retained transport
independently every five seconds. This watch survives completion of the main
process's request. A failed main-process SIM refresh therefore cannot prevent
the original owner from observing its own lost socket. Stop and close cancel and
join that watch before cleaning up; an unfinished join retains the capability.

## Boundaries

This does not recover a dead independent owner or reconstruct its capability
from disk. A consumed/uncertain restart intent, unavailable secondary port,
changed policy/SIM, RF-off state or unconfirmed cleanup keeps the fence intact
and can require operator maintenance. It is not a generic reset on every network
failure and does not authorize a restart while another protected access is
configured. Caller disconnect or an owner restart cannot replay an old intent.

The [combined checks and actual automatic recovery](VERIFICATION_20261027_PROXY_AUTO.md)
record one successful run with the original main/owner retained and an IPv6
public query. Earlier failed phases and CFUN acknowledgments without observed
generation change are retained there. That acceptance is separate from the
earlier [fault-containment/manual-maintenance run](VERIFICATION_20261027_PROXY_LOSS.md)
and does not establish recovery after owner death or with default supervision.
