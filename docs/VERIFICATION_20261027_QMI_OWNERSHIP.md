# Real QMI status/ownership inquiry — 2026-10-27

This diagnostic continues the [crash/reconnect work](VERIFICATION_20261027_CRASH.md).
It determines what a fresh WDS client can observe before implementing ordinary
data crash recovery. **No application crash was injected, and automatic data
recovery is not established by this run.**

## Original hardware and independent observer

The unchanged application runtime from `7ed6ea2` used the original QDC507,
authorized China Telecom SIM and existing CTNET APN. The binary SHA-256 was
`4fea12ecb7153f68c70929878593fc2bb71c7ba87bd48577a5eec383fcb66a8f`.
A separate private Go observer used the same pinned QMI library, the original
proxy and freshly allocated WDS clients. It selected each observer client's IP
family and read packet status/runtime settings. It never started/stopped packet
data, changed WDA/RF, wrote modem profiles or adopted an earlier CID/handle.
Original ICCID/IMSI and USB generation were checked before and after each probe.

The application alone enabled and disabled its owned data connections. Each
probe allocated and explicitly released only its own clients. The observer's
proxy-only configuration disabled raw-device fallback and CTL Sync on open.

| Application state | Fresh IPv4 client | Fresh IPv6 client |
| --- | --- | --- |
| Before data | Disconnected; runtime unavailable | Disconnected; runtime unavailable |
| IPv6 connected | Disconnected; runtime unavailable | Connected; valid IPv6 runtime |
| After IPv6 Stop | Disconnected; runtime unavailable | Disconnected; runtime unavailable |
| IPv4 connected | Connected; valid IPv4 runtime | Disconnected; runtime unavailable |
| After IPv4 Stop | Disconnected; runtime unavailable | Disconnected; runtime unavailable |
| Dual stack connected | Connected; valid IPv4 runtime | Connected; valid IPv6 runtime |
| After dual-stack Stop | Disconnected; runtime unavailable | Disconnected; runtime unavailable |

All **14 packet-status observations** succeeded. Runtime-setting failures on
inactive families were expected and remain distinct from status-query failures.
After the dual-stack probe, the application still reported connected and had a
populated public-IP cache. This does not identify or transfer the original WDS
lifecycle capability. In this modem/firmware scenario, a fresh client's status
can describe an existing packet call that the observer never started.

## Consequence for crash recovery

The actual proxy peer was observed as `qmi-proxy 1.38.0`. Its
[versioned upstream source](https://github.com/linux-mobile-broadband/libqmi/blob/1.38.0/src/libqmi-glib/qmi-proxy.c)
places clients that were not explicitly released into a disowned-client list
when their application disconnects. An application-process exit therefore
does not supply an acknowledged cleanup result for its former packet call.
This source finding is not a claim that a particular PDN survived a crash here;
that fault was not injected.

The existing SIM-bound/multi-family path correctly treats an already connected
packet status as a cleanup fence. The stale comment describing this path as
StopAny-based restart recovery was corrected. Runtime behavior was unchanged;
Go, frontend and kernel suites were not repeated for a comment/documentation
change. The previous [software evidence](VERIFICATION_20261027_NATIVE.md) remains
bound to its executed source snapshot and binary.

Recovery implementation must first preserve durable original ownership of the
bearer and host configuration. A fresh CID, a readable address, a matching APN
or an online interface cannot substitute for the original cleanup capability.
The next implementation needs a stable bearer owner and exact journaled host
objects, followed by a real crash with the host namespace retained. The existing
ModemManager-backed Host IMS mechanism is a candidate to evaluate for ordinary
data; its IMS APN/P-CSCF authorization boundaries must remain intact.

## Restoration and remaining work

Each application data-off reached `disabled`; original rules, main defaults,
IPv6 settings, MTU and link flags were restored, with no owned global address
left. The application exited with code 0. The matching temporary ModemManager
build retained the same process and restart count through the data probes and
port-owner release, then the original distribution binary/service was restored.
Only owned temporary binary/drop-in/inhibitors were removed or released. Both
original modem/SIM pairs registered with RF on, original database bytes and
running container IDs were unchanged, host XFRM state/policy lists were empty
and the shared-lock inode was preserved. No modem reboot, firmware handoff,
carrier call or mobile SMS was submitted.

The [machine-readable record](verification-20261027-qmi-ownership.json) includes
all observations, probe/application hashes and restoration checks. Actual
addresses, CIDs, subscriber identities, credentials and raw logs remain private.
The [full acceptance ledger](PARITY_ACCEPTANCE.md) remains open, including Host
IMS and ordinary-data crash recovery. The requested Host IMS maintenance window
has not yet been approved; this run did not perform that handoff.
