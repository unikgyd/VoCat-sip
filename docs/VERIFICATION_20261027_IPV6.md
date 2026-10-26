# Real IPv6 and dual-stack export acceptance — 2026-10-27

This continues the [IPv4 data/shutdown run](VERIFICATION_20261027_NETWORK.md),
from source baseline `d3c7b8f`. The overall Linux hardware goal remains open.

## Changes required by the tests

The old export path mapped `IPV4V6` to IPv4 and configured IPv4 DHCP even for
an IPv6-only WDS request. Native data now implements IPv6 runtime addressing,
gateway/DNS, marked routing and readiness. Dual stack retains separate original
IPv4/IPv6 WDS clients and handles; both must be ready. Unknown starts and failed
stops retain the affected client. A changed APN/family/credential request cannot
reuse the previous connection without acknowledged cleanup.

The first deployed IPv6 candidate established its bearer and resolved carrier
DNS, but domain-based proxy connections timed out. A literal IPv6 CONNECT
completed in 0.36 seconds. Review found serial address attempts, which could
exhaust the connection budget on the host's preferred address family before
trying the modem's usable family. The final candidate uses `net.Dialer` family
fallback with the original bound socket controls and carrier resolver. A real
dual-family loopback/DNS test stalls the first socket setup and verifies that
the other family connects without losing those controls.

IPv6/dual-stack cleanup removes installed addresses, preserves routes on other
interfaces and restores IPv6 settings, MTU and administrative link state. The
three new opt-in kernel cases exercise actual Linux addresses/routes and
rollback in private network/mount namespaces, including foreign addresses and
replacement-device refusal. Ambiguous WDS starts remain software-fixture
evidence, rather than injected carrier faults.

## Final candidate on the original modem and SIM

The original QDC507 and authorized China Telecom SIM used their existing
`CTNET` APN. Cellular/Wi-Fi automatic startup stayed disabled. No radio reset,
carrier call or mobile SMS was submitted. Existing native non-voice CLCC
records were preserved.

An independent Windows client used authenticated HTTP CONNECT and SOCKS5
proxies on the NAS, with normal TLS verification and bounded response bodies.
Successful responses reported the required public address family. Header-only
observation pinned the original USB generation and interface index.

| Requested mode | Successful real HTTPS transfers | Observed TCP/443 headers, outbound / inbound |
| --- | --- | --- |
| IPv6 | HTTP by domain and SOCKS5 by IPv6 literal; both reported IPv6 | IPv6: 28 / 25 |
| IPV4V6 | HTTP and SOCKS5 over IPv4, plus SOCKS5 over IPv6 | IPv4: 35 / 34; IPv6: 15 / 12 |
| IPv4 regression | HTTP by domain and SOCKS5 by IPv4 literal | IPv4: 40 / 36 |

Wrong credentials were rejected in each mode. Data-off returned 409 while the
test proxies were enabled. Disabling only those proxies closed both held
tunnels. Data-off then reached `disabled`, cleared public-IP detection, removed
owned addresses/private DNS and restored the original IPv4/IPv6 policy rules,
main default routes, MTU, link-up state and IPv6 settings for each scenario.
Observed IP bytes are transport evidence, not a carrier billing measurement.

The fast IPv6 transfer window canceled its automatic public-IP query when data
was stopped; it did not establish a positive IPv6 public-IP API result. Dual
stack and IPv4 produced automatic lookup-success events. Cache invalidation
after data-off was checked in all three modes.

The [machine-readable record](verification-20261027-ipv6.json) contains all
client outcomes, header counts, software checks, artifact hashes and rollback.
Subscriber identities, public addresses, credentials, full packet headers,
database backups and failed-attempt details remain private.

## Software and shipping configuration

The final frozen Go source passed **2,057 Go tests**, with zero failures and
50 opt-in/environment skips, vet, **30 selected race-test executions** (ten
top-level cases repeated three times), the **three opt-in kernel cases** and
Linux amd64/arm64 builds. The general run used `CGO_ENABLED=0`; race checks used
`CGO_ENABLED=1`. The earlier frontend/other kernel runs were not counted again.
The deployed amd64 binary was copied back and verified as
`f4cf9537fd0d5500e134232922a7e860e70f0134de0479d20ff8cc839369b42a`.

Shipping permissions were checked separately from the frozen Go source. The
unit and installer retain kernel protection with writable exceptions for owned
device setup, IPv6 interface settings and shared reservations. Compose keeps
the sysfs root read-only and overlays `/sys/devices` writable. Both service
definitions and compose now allow 120 seconds for the separate shutdown stages.

Bash syntax, systemd unit syntax and compose configuration checks passed. A
temporary native service reused the shipping hardening and opened write-capable
descriptors for the original modem's raw-IP/IPv6 attributes, without writing any
attribute value. It confirmed private runtime-directory mode, shared lock access
and that unrelated kernel tunables remained read-only. A separate container
checked the compose sysfs mounts the same way. The unit syntax check substituted
an installed command-path fixture; these probes do not establish a complete
native installer/service lifecycle.

## Dependency and restoration evidence

The matching ModemManager `1.25.95` build with the existing
[netlink completion patch](patches/modemmanager-1.25.95-netlink-completion.patch)
retained the same process through all data tests and release of both legacy-port
inhibitors, with **zero automatic restarts during that window**. This validates
the controlled patched dependency for these scenarios; it does not erase the
distribution binary's earlier crash or prove its cause from a missing core.

The first temporary ExecStart pointed under `/root` and failed with status 203
because `ProtectHome=yes` hides that directory. The verified executable was
placed under `/usr/local/libexec`, preserving the service hardening. The local
Windows resolver also omitted AAAA answers, so literal test targets were resolved
through the pinned carrier DNS instead. An early client helper shadowed its HTTP
module name. These retained setup/helper failures are not passing product checks.

After acknowledged cleanup, the application exited with code 0. Original private
device/card preferences and proxy configuration were restored with the test
application stopped; incoming messages and SIP ledgers were preserved. Only
verified test inhibitors were released. The temporary manager executable/drop-in
were removed, the original binary/service was restored, both original modem/SIM
pairs registered with RF on and original running Docker container IDs remained
unchanged. Host IMS claims, task recovery journals and XFRM states/policies were
zero. The earlier automatic-restart history remains in the prior report/logs.

## Still open

Answered/incoming carrier voice, carrier hold/DTMF/recording/playback/browser
takeover and mobile SMS delivery need controlled endpoint participation. Physical
hotplug, abrupt crash recovery, successful task business execution, browser work
past the first-use agreement, external notification-provider delivery and
disposable eSIM provisioning/download/delete retain their separate acceptance.
Positive IPv6 public-IP API acceptance was subsequently completed, with a
provider-endpoint repair and real native upgrade rollback, in the
[native installation run](VERIFICATION_20261027_NATIVE.md).
VoWiFi carrier calls remain outside the requested scope.
