# Standard installer with an empty database — 2026-10-27

The unmodified standard installer completed a first installation from an absent
database on the NAS, created a random administrator, printed its password once,
and started a healthy native systemd service. A subsequent forced reinstall
preserved the exact administrator, original password and a pre-reinstall login
session. This closes the combined installer/first-administrator scenario under
the explicit hardware isolation below.

The [machine-readable record](verification-20261027-fresh-installer.json)
contains the scoped checks, retained dependency changes and observation repairs.
This is not a fresh operating-system image or public release delivery, and the
full hardware acceptance objective remains open.

## Candidate and isolation

The installed amd64 artifact was unchanged from the preceding checked candidate:
`5e9ab66648d0442fb4a0bb46c088c009f217cad8c08553b3adc4ac2432bf7044`.
All 1,774 implementation/test/frontend/script/module paths match its frozen
manifest; the installer script also matches byte for byte. The prior 2,096 Go
passes, 15 selected race executions, three kernel cases, vet, four builds and
82 frontend cases remain [referenced evidence](VERIFICATION_20261027_ESIM_TASKS.md),
not new executions in this phase.

The standard application, configuration, unit and command-link paths were absent
before setup. No database or administrator was seeded. A loopback listen address
was supplied through the installer's normal environment file. The installer
created the database and generated its own unit without source edits. An explicit
additional service drop-in enabled `PrivateDevices` and hid modem-discovery
sysfs paths, host D-Bus and the PC/SC socket. The resulting application inventory
was empty; no modem, SIM, RF, data or carrier acceptance is inferred from this
first-install service.

Only the exact checked binary and matching checksum download URLs were served
by a private transport fixture. Health calls used real curl and the actual native
service. Existing NAS systemd, QMI and host prerequisites were used. Neither
Internet GitHub release delivery nor OpenWrt/procd installation was exercised.

## Real installation outcomes

| Check | Observed result |
| --- | --- |
| First standard installer | Exit 0; database initially absent; no seed or copied administrator |
| Generated administrator | Exactly one administrator; random initial password printed once |
| Credential storage | Database and environment mode `0600`; administrator password absent from environment |
| Anonymous device API | HTTP 401 |
| Login with generated password and session lookup | HTTP 200 |
| Actual native service | Healthy; checked executable; zero automatic restarts; isolated inventory empty |
| Forced standard reinstall | Exit 0; exact administrator record and password preserved; no initial password printed again |
| Session created before reinstall | Still authenticated afterward |
| Successful replacement lifecycle | New service invocation; zero automatic restarts; previous-binary backup removed after health checks |
| Final application stop | Exit 0; SQLite integrity `ok`; zero device rows |

The first installer ran exactly once. An early observer queried `users` instead
of `admins`, then needed a JSON representation of the binary password hash. The
device-list observer also initially checked the response envelope instead of its
`devices` array. Each was corrected by reading the same completed installation;
none required another first-install or bootstrap command.

A cleanup observer initially compared complete `ExecStart` display metadata.
`systemctl daemon-reload` had cleared cached command PID/time fields even though
the original ModemManager PID and automatic-restart count were unchanged. Final
attestation used those live process properties and the original executable.
The temporary application had already stopped and been archived; cleanup resumed
without reinstalling or starting it again. These are observer repairs, not
product failures relabeled as passing runs.

## Native dependencies

The standard installer installed distribution `pcscd` and `libccid`. It also
correctly warned that the host lacked the AMR-NB library: the checked binary's
`media-check --require-amr --json` initially exited 1 with AMR-NB unavailable.
The installer's stated Debian/Ubuntu remedy was then exercised. A package-manager
simulation confirmed that only `libopencore-amrnb0` would be added; installation
succeeded, and the same binary's required-AMR diagnostic then exited 0 with
AMR-NB available. No existing package was upgraded.

The three prerequisite packages were deliberately retained:

| Package | Observed installed version |
| --- | --- |
| `pcscd` | `2.4.1-1` |
| `libccid` | `1.7.1-1` |
| `libopencore-amrnb0` | `0.1.6-1build2` |

PC/SC socket enablement/state is recorded in the JSON. Package and PC/SC rollback
is not claimed. AMR-WB remains unavailable in this artifact. This diagnostic is
not a new native carrier call or proof of browser audio devices.

## Final state and remaining acceptance

The owned application installation, generated unit, enablement link and isolation
drop-in were removed from the standard paths after a clean stop and archived
privately. Both original SIM/equipment pairs remained registered with RF on;
ModemManager PID/restart count, both USB generations, exact host network snapshots,
original database hash, shared-lock inode and running-container IDs were retained.
No XFRM state remained. No firmware write, modem reboot, carrier call or SMS was
requested. The Philippines SIM made zero calls and sent zero SMS.

This covers the standard installer and empty-database administrator lifecycle
on this NAS with isolated hardware. Fresh-install discovery/default SIM behavior,
a clean operating-system dependency matrix, public release delivery, actual browser
microphone/speaker use, EULA interaction, remote IVR recognition, carrier SIP
bridging, recipient-confirmed IMS SMS replies, independent owner/proxy loss,
physical hotplug and host failure still need their own acceptance. Passwords,
hashes, installer logs, database copies and private fixture scripts stay outside Git.
