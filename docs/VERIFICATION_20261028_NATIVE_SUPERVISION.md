# Native supervision recovery and deployment readiness — 2026-10-28

The native optional three-service layout now preserves its clients across a
proxy crash. Real CT dual-stack data recovered after separate active-owner and
proxy SIGKILLs. The [machine-readable record](verification-20261028-native-supervision.json)
contains the source/unit hashes, exact scope and restoration evidence. The
[deployment runbook](DEPLOYMENT_RUNBOOK.zh-CN.md) provides executable installation,
verification, backup, upgrade and rollback steps.

## Defect and change

An isolated real systemd dependency diagnostic reproduced the previous layout:
the proxy automatically restarted, but `Requires=` left both clients inactive.
The diagnostic used sleeping clients in private network namespaces and is not
carrier acceptance. Its results and a cross-filesystem archive observer failure
were retained; only the owned diagnostic units were subsequently removed.

The owner unit and main drop-in now use `Wants=` plus ordering. Each client also
requires both a successful dedicated-proxy `systemctl is-active` check and the
real `vocat qmi-proxy --check` before initial execution. Runtime proxy failure
therefore leaves original cleanup capabilities alive; initial startup still
refuses an unavailable proxy. Restart delays, shutdown budgets, shared locks,
namespace layout and protection settings are unchanged.

Only `deploy/vocat-data-owner.service` and `deploy/vocat-data-owner.conf` changed
in product inputs from commit `044fcfe080df4c2710fb277732e60136cf653f21`.
All other 1,825 frozen non-documentation inputs, including Go and embedded web
assets, matched the preceding checked source. The units are not embedded in the
executable. The exact unchanged amd64 binary was:
`403d5708de465c3575ee8118af35a9e0d2155c1bf69f581d87310e99eef04ad1`.

The preceding 2,140 Go passes / 50 skips, seven isolated kernel cases,
35 kernel race executions, vet and four architecture builds are explicitly
referenced, not rerun or attributed to a new binary. The new configuration was
validated with `systemd-analyze verify` and actual unit behavior after editing
was complete.

## Configuration checks

The real restricted proxy ran in an owned, pinned network namespace. Client
commands wrote a marker and slept in place of opening hardware, preserving
the source protection and startup checks with adapted unit/file paths.

| Actual isolated behavior | Result |
| --- | --- |
| Proxy SIGKILL | One automatic proxy restart; both client PIDs/invocations retained |
| Owner SIGKILL | One automatic owner restart; main and proxy retained |
| Another serving proxy prevents dedicated proxy startup | Foreign listener preserved; neither client command executed |
| Dedicated service active without a listening socket | Neither client command executed |

The negative startup fixture's stop interrupted a precheck and left a failed
unit state. That observer assumption was retained; zero-PID stopped states were
recorded before resetting only these disposable negative units. All diagnostic
units, processes, namespace and runtime paths were removed. These checks prove
supervision/startup behavior, not modem recovery.

## Actual native hardware cases

The four frozen deployment files were installed at their normal native paths.
The original authenticated database was copied into an isolated test install;
task/notification/SIP/access activation was disabled and ordinary data was
enabled only for the original CT SIM. The owner received an explicit, verified
stable secondary AT mapping. Matching patched ModemManager was used; native IMS
remained `1,1`. No serving foreign proxy was adopted or stopped.

Both cases began with a ready version 2 claim, a successful live public query,
and observed Unix transport between the original owner and dedicated proxy.

| Actual active-session fault | Automatic data and fresh public query | Public family |
| --- | --- | --- |
| Independent owner SIGKILL | 37.329 s | IPv4 |
| Dedicated proxy SIGKILL | 67.365 s | IPv6 |

Each exact target PID was killed once through a PIDfd. Systemd recorded
killed/status 9 and one automatic restart. Every other original service kept
its PID, invocation and restart count. A durable original recovery intent,
old USB generation loss, old claim deletion, empty original rule/DNS boundary,
fresh ready claim and HTTP-200 query were observed. No operator reset, restart,
enable or fence deletion occurred inside either fault window.

The proxy case captured one complete 12-byte CFUN write on the configured
secondary port. The owner case did not capture that syscall; its narrower
evidence is intent, generation loss, cleanup and reconnection. No byte-level
submission count is claimed for that case. No carrier call or SMS was sent,
including zero Philippines calls/SMS. No voice bridge was used.

This accepts these two native **dual-stack data-only** scenarios with explicit
recovery configuration. It does not accept every standalone IPv4/IPv6 fault
combination or protected Host IMS/SIP joint fault. Automatic ordinary-data
restarts remain prohibited by policy when protected access is configured.

## Restoration

Data was disabled normally and both claim types were absent. Main and owner
recorded normal exit 0; the owned proxy had no established clients and stopped
successfully. An overly strict cleanup observer expected a particular detailed
proxy exit code; detailed fields were zero at reconciliation, so they could not
establish that code.
The failed observation was retained and reconciled using zero PIDs, success
state and successful-deactivation journal evidence; no additional restart or
hardware recovery was issued.

All temporary native install paths, unit/drop-in files, configuration, private
database and patched manager copy were archived. Both pre-existing lock inodes
were preserved. Original equipment/SIMs registered with RF on, and strict
query-frame parsing confirmed native IMS 1 and all original PDP fields under
patched MM before switching back. No stock-manager inhibition/AT cycle followed.

The original stock manager then stayed at one PID with zero automatic restarts
for 121.685 seconds. Original database hash, five running production container
IDs, PC/SC state, PH host state and CT host state except recreated interface
index matched. There were no claims, private DNS, XFRM objects, test native
services, sockets or new runtime paths left behind.

## Release decision

The candidate and deployment instructions are ready for controlled target-host
qualification. Unconditional production readiness and the overall hardware goal
are **not** claimed. A verified matching ModemManager dependency remains required;
the earlier distribution-manager SIGSEGV is unresolved. IPv6 cold-start behavior
previously needed separate module maintenance and is not certified as automatic
repair. Native family/fault combinations, power/host loss, physical hotplug and
carrier-bound joint faults retain their own acceptance. Browser physical audio
remains deferred by the user.
