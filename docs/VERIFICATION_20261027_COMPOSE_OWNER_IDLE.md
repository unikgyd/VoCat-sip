# Shipping Compose idle-owner restart — 2026-10-27

The shipping Compose layout passed one actual **idle** data-owner SIGKILL and
automatic restart, followed by an explicitly requested fresh data connection and
an IPv6 public-address query. This does not recover a dead owner with an active
QMI capability. The [machine-readable record](verification-20261027-compose-owner-idle.json)
contains the fault, subsequent connection and restoration checks.

## Unchanged candidate and deployment

Main revision `165b50d3eb9f72eca4c0d7761c4a86d6c010283f` still matched all 1,784
selected inputs of the [previous frozen software candidate](VERIFICATION_20261027_PROXY_AUTO.md).
Its amd64 artifact was
`cf7c11fedba7aa58435180fda225fd44692d5294a22956f55e14171deccfdc20`.
No implementation changed and no new Go/frontend/build execution is claimed.

Docker Compose v5.6.0 executed exact copies of `docker-compose.yml` and
`compose.data-owner.yaml`. A private override selected existing local images,
the read-only checked binary, private database/SIM admission file, addresses,
socket path and container names. The main image's actual default entrypoint ran.
The shipping `unless-stopped` policies, proxy health check/dependency gates,
host networking/PID, device/sysfs mounts and 120-second client stop grace stayed
in effect. No recovery-port mapping was configured. Public image delivery and
building the release image are outside this existing-image fixture.

## Actual fault and fresh connection

Before the fault, ordinary data was disabled, the owner had no established RPC
peer or data claim, and Host IMS/VoWiFi were disabled in the fixture. Only the
exact original owner process received SIGKILL. Exit 137 and one Docker automatic
restart were observed. The replacement process remained stable for a 12-second
dwell; the complete fault/replacement observation took 13.835 seconds. This is
an observation window, not a measured media or network outage.

The original main and proxy processes remained unchanged with zero restarts.
The replacement owner used the same checked binary, recreated its Unix socket
with mode 0600 and retained the singleton lock file inode. No data claim appeared
during the idle fault. Host IMS claims were checked before and after the fault;
this is not a continuous Host IMS claim trace. The physical USB generation did
not change and no modem reset or manual process restart was submitted.

Only after that observation, a new explicit data enable requested CTNET dual
stack. The replacement owner created its own new claim for the original physical
equipment/SIM and unchanged USB generation. No previous capability was adopted
and no recovery intent was consumed. A real public-address query returned HTTP
200 with IPv6. The original main/proxy and replacement owner still retained their
process identities. Explicit disable then confirmed claim, route, address and
private DNS cleanup.

## Restoration and remaining scope

Actual Compose stops left all three containers exited with code 0; restart
counts remained main 0, owner 1 and proxy 0. Private socket/runtime state was
archived. Both original equipment/SIM pairs registered with RF on; native CT
IMS remained `1,1`. Native IMS/PDP/USB-generation records and both host network
snapshots matched their pre-run state, including the CT interface index.

The stock manager was restored with zero automatic restarts. Its temporary
override/executable were removed. The original database hash, shared lock inode,
running container IDs and host PC/SC service/socket state were preserved. No
owned Compose network or volume remained, and no data claim, XFRM state/policy
or private DNS file remained. This phase placed **zero calls and zero SMS** on
both SIMs.

Active-claim owner death, proxy-loss supervision cascades, daemon/host failure
and power-loss/orphan-resource restoration remain separate scenarios. The
earlier surviving-owner proxy recovery cannot establish those guarantees.
