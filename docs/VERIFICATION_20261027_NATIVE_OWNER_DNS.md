# Native owner, DNS and transport lifecycle — 2026-10-27

Real native systemd main/owner services completed the IPv4 and dual-stack/IPv4
recovery cases below. The run also found and repaired a cellular DNS selection
bug. A later cold-transport IPv6 crash exposed a separate QMI proxy lifecycle
failure. That failure is retained and is not a recovery pass.

The [pre-maintenance record](verification-20261027-native-owner-dns.json)
describes the completed, restored phases before the explicitly approved modem
reboot. Its `modemResets: false` applies to those phases only. Subsequent
maintenance, transport-loss results and final corrected software checks have a
separate [machine-readable record](verification-20261027-native-owner-maintenance.json).

## Actual native services

The executed main unit, owner unit and main drop-in were byte-identical to the
frozen source based on `557b2b6`. Both processes used the same checked binary,
private database, original administrator and original host network namespace.
The actual unit protections and capability/mount settings were inspected after
startup. This extends the earlier Docker owner cases to native execution; the
standard installer still does not enable the optional owner layout itself.

| Candidate and saved mode | Actual main SIGKILL through public-IP success | Result |
| --- | --- | --- |
| Initial native candidate, IPv4 | 28.914 s | HTTP 200, IPv4 |
| DNS-corrected candidate, dual stack | 29.875 s | HTTP 200, IPv4 |
| DNS-corrected candidate, IPv4 | 62.218 s | HTTP 200, IPv4 |

Every complete case verified systemd's killed/status-9 result, automatic main
restart, unchanged original owner PID/invocation, unchanged host namespace and
ModemManager, original flock retention, old-claim deletion and restored host
state before a fresh claim for the same SIM/USB generation. No enable or restart
request was sent after the injected crash. IPv4's slower case and its 134
transient failed-status samples remain in the record; samples are approximately
250 ms apart and do not represent 134 separate modem failures.

Normal data-off cleared each claim and restored interface settings, rules,
default routes and DNS state. Both services exited zero. The original database,
registered SIM/equipment pairs, running container IDs, distribution ModemManager,
lock inode and owned deployment paths were verified after each completed phase.

## Reproduced DNS fault and correction

Both carrier-provided IPv6 DNS servers received queries without replying; both
IPv4 DNS servers answered. The old resolver accepted the first successful UDP
connect, which does not demonstrate any DNS response. Consequently it repeatedly
selected the same silent IPv6 server and returned HTTP 502 even for usable dual
stack data. The initial IPv6 crash reconnected ownership but still failed the
public-IP query; it is not included among the complete cases above.

The corrected [cellular resolver](CELLULAR_DNS.md) races actual exchanges across
the bounded configured server list, retaining the original interface/route mark.
Response matching, SERVFAIL/REFUSED handling, UDP-to-TCP truncation, full TCP
framing, cancellation and joining losing workers have socket-level regressions.
There is no host-WAN DNS fallback or change to original WDS ownership.

The same live failure pattern was reproduced after correction. The application
returned HTTP 200 over cellular IPv4 while IPv6 DNS remained silent. Captures
showed IPv4 DNS and HTTPS replies on the modem interface. An independent Windows
client also completed authenticated HTTP CONNECT and SOCKS5 domain-name HTTPS
requests with verified TLS. Wrong authentication was rejected, existing tunnels
closed on proxy disable, listeners were deleted, and active-proxy data-off was
correctly rejected with HTTP 409. Original proxy configuration was restored.

The completed DNS snapshot passed 2,081 Go cases, 75 selected race executions,
three explicit isolated kernel cases, vet and amd64/arm64 builds. Native 386 and
armv7 release builds also passed without hardware execution. There were 50
environment/opt-in skips. Source and artifact hashes are in the JSON record.

## Approved reboot and retained transport failure

Before maintenance, IPv6-only DNS and a separately marked/interface-bound
literal IPv6 TCP control both timed out. Dual stack worked through IPv4 after
the DNS repair. All test connections and native services were released and the
original environment restored before requesting a new maintenance window.

The operator approved exactly one QDC507 reboot while retaining native IMS mode
1. The command was sent once and acknowledged. The initial observer stopped on
the transient disappearance of the USB path; read-only continuation confirmed
the new USB generation, same original equipment/SIM and registered native IMS
`1,1`. No IMS setting write or replayed reboot occurred. Serial-read and
continuation-script errors before deployment setup are retained in private logs.

On the same checked DNS artifact, fresh IPv6-only data and the real public-IP
query then returned HTTP 200 with an IPv6 address. Modem-interface IPv6 DNS
queries and replies were captured. This is recovery after explicit maintenance;
it does not prove that DNS software repaired the original IPv6 packet-path fault.

The subsequent actual IPv6 main SIGKILL did **not** recover within 180 seconds.
The owner survived, but its original proxy sockets returned Broken pipe and no
QMI proxy remained. Original host addresses/rules were removed; packet cleanup
could not be acknowledged, so the original private record and instance flock
stayed retained. New main starts were correctly denied. The retrying main service
was stopped for diagnosis while the original owner was preserved. Read-only
attestation still confirmed the original CT SIM and native IMS `1,1`.

A separate real systemd control reproduced the transport lifecycle problem
without sending QMI requests or touching a modem: a proxy launched in a detached
session still belonged to the main unit's cgroup and was killed after that
unit's main PID received SIGKILL. Detaching a process group does not escape
systemd's default control-group cleanup.

The correction adds a dedicated `vocat-qmi-proxy.service` and an equivalent
Compose proxy container. `vocat qmi-proxy` executes the installed libqmi proxy
with `--no-exit`; a real socket readiness check orders both clients after it.
An existing serving proxy is refused rather than adopted or killed. Cleanup
errors are now logged while the original capability and flock remain retained.
See [deployment and limits](DATA_OWNER.md). Cold native/carrier acceptance of
this new layout and complete restoration of the failed phase are pending.

The final corrected snapshot passed a fresh full run: **2,081 Go cases, 75 race
executions, three isolated kernel cases, vet and all four Linux builds**. There
were 50 environment/opt-in skips. Compose configuration, shared client state and
both readiness dependencies passed; all three native units passed systemd
syntax verification. In an isolated network namespace, the real launcher kept
its PID when executing libqmi, passed readiness, refused an existing proxy without
replacement, survived an independent client's SIGKILL, and failed readiness when
the proxy was absent. No QMI request or modem operation was used for these
launcher checks. They do not substitute for the pending cold carrier scenario.

## Remaining scope

Proxy loss is not repaired by starting a fresh proxy or reconstructing CIDs.
The failed original owner remains fenced until controlled recovery proves the
old physical generation is gone and that owner acknowledges release. Another
modem reset requires separate operator authorization; the first approved reset
was already consumed. No claim is forcibly removed to continue testing.

Loss of the independent owner, daemon/host/power loss, answered carrier audio,
mobile SMS and the other [open acceptance scenarios](PARITY_ACCEPTANCE.md) remain
separate. No carrier call, mobile SMS or VoWiFi call occurred in this run.
