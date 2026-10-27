# Native cold-proxy carrier recovery — 2026-10-27

The checked `f5eea23` candidate completed actual main-process SIGKILL recovery
with all three unmodified shipping units: `vocat`, `vocat-data-owner` and
`vocat-qmi-proxy`. The [public record](verification-20261027-native-cold.json)
contains the frozen source/artifact hashes, results and restoration assertions.
This follows the [independent deployment checks](VERIFICATION_20261027_NATIVE_INDEPENDENT.md).

## Earlier failed phase restored

The separately authorized recovery reboot was issued once, keeping native IMS
at `1`. The same original CT SIM/equipment returned under a new USB generation
with native IMS `1,1`. Its surviving original owner observed disappearance of
the old generation, acknowledged cleanup, deleted its own claim and released
the original flock. No claim was forcibly deleted and no new client adopted a
lost WDS capability. The original raw-IP/link state and installation environment
were restored. This was controlled recovery; the earlier failed automatic case
remains a failure in its historical report.

## Three native services on the real host

Both original modems were positively inhibited and the existing proxy had no
live client connections. Its owning ModemManager service was stopped normally.
After confirming process/socket absence, the dedicated proxy was cold-started
before ModemManager. The original SIM/equipment pairs were reconfirmed, inhibited
again and handed to the checked application. No serving proxy was adopted.

| Real saved-intent case | Recovery and public query | Public address |
| --- | --- | --- |
| IPv4 main SIGKILL | 28.936 s | IPv4 |
| IPv6 main SIGKILL | 28.883 s | IPv6 |
| Dual-stack main SIGKILL | 29.951 s | IPv4 |

Every case recorded systemd killed/status `9`, a new main PID/invocation and
one automatic restart. The original owner, independent proxy, ModemManager,
host network namespace and same-case SIM/USB generation remained unchanged.
Live Unix connections proved that the owner used the independent proxy before
and after the crash. Original claim deletion and empty host state preceded a
fresh claim and HTTP 200 public query. No enable or restart request followed
the crash. Each final disconnect restored the original host state.

## IPv6 prerequisite and retained observations

The first IPv6 WDS connection had outbound DNS and no replies, producing HTTP
502. That result was not accepted through IPv4 fallback. A separate authorized
diagnostic reboot, with IMS kept at `1` and the same ModemManager continuously
inhibited, restored real IPv6 reachability before the IPv6 crash case. The
initial reinitialization fault remains open; this maintenance action is not an
automatic repair of that initial fault.

The post-reset observer initially omitted the original `MULTICAST` interface
flag. It resumed from the verified new generation without replaying the reboot.
Final container-set comparison also saw an unrelated database container started
during the run. Every original container ID was unchanged; the unrelated new
container was preserved. Both observer failures remain in private evidence.

All three owned services stopped successfully. Temporary native paths and the
patched ModemManager copy/drop-in were removed, the distribution manager was
restored, and both original SIMs returned registered with RF on. The original
database, network, lock inode and original containers were retained.

These cases sent no carrier call or SMS. Non-voice CLCC entries on both modules
also stopped a registration-task preflight before task creation; that exposed
a separate voice-idle classification defect. Owner/proxy loss, machine loss,
answered media, successful scheduled work and mobile SMS delivery retain their
own acceptance requirements in [the ledger](PARITY_ACCEPTANCE.md).
