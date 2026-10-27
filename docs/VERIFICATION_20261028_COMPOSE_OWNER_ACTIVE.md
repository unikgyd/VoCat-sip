# Active data-owner death under shipping Compose — 2026-10-28

An acknowledged, SIM-bound ordinary-data session now has a separate recovery
path after its independent owner dies. Actual CT IPv4, IPv6 and dual-stack owner
SIGKILLs recovered automatically under the shipping Compose restart policies.
The original main process and QMI proxy survived every fault window. This closes
the active-owner gap left by the [proxy-only cases](VERIFICATION_20261028_COMPOSE_PROXY_ACTIVE.md)
within the conditions below. It does not establish whole-platform completion.
See the [sanitized evidence](verification-20261028-compose-owner-active.json).

The candidate was frozen from baseline `3df1727c4d93fcd4a5c514b2835239b09b274bf3`.
Final source manifest SHA-256:
`70c48c2c09a6cd292882fffcd8b37a315b81c7ec7343a605aaa5fef6cfb074d9`.
The actual amd64 executable was
`403d5708de465c3575ee8118af35a9e0d2155c1bf69f581d87310e99eef04ad1`.
Implementation and regression cases were completed before each combined check
batch; no implementation changed after the final batch.

## Recovery boundary

Version 2 of the private data record adds original process start ticks, boot,
PID/user/network namespaces, UID, USB path and a durable host receipt. The owner
first reserves an empty interface/routing/DNS slot, marks configuration pending,
then commits exact rules, route hashes, addresses and DNS file/directory identity
before acknowledging an active session. Reconfiguration becomes pending again.
The record contains no reusable WDS CID or packet handle.

The replacement singleton takes the original equipment lock and proves original
process death within the same boot and namespaces. A configured secondary AT
port must still belong to the original USB instance. Fresh equipment/SIM,
data-only policy, online RF and absence of voice calls are checked before and
after the single FULL-synchronous restart intent. A missing or uncertain reply
cannot replay that intent. The replacement retains an uncertain serial dialogue.

Claim deletion additionally requires original USB generation loss, absence of
the original kernel interface, empty original private routing tables, and exact
original rule/DNS identity. Changed selectors, actions, aliases or DNS objects
refuse cleanup. No replacement interface is flushed or brought down. Only after
confirmed cleanup can saved data intent acquire a fresh session.

The pinned netlink library omits rule actions in its decoded rule listing. The
new reader validates the original kernel action and attributes, including the
extended table number and its `RT_TABLE_COMPAT` header. Unknown selectors fail
closed; matching only mark/priority is insufficient.

## Combined software checks

| Final frozen candidate | Result |
| --- | --- |
| Complete Go suite | 2,140 passed; 50 skipped; no failures |
| Actual private network/mount namespace data cases | 7 passed; no skips |
| Those kernel cases with race detector, five iterations each | 35 passed; no skips |
| `go vet ./...` | Passed |
| Linux amd64, arm64, 386, armv7 builds | All passed |

An additional 200 owner/recovery race executions passed at five iterations per
root in the preceding frozen batch, with 10 opt-in skips. All its inputs except
the kernel rule reader match the final manifest. The final batch re-executed
that changed reader through the real kernel cases with the race detector.
This is an explicit unchanged-source reference, not a claim that the preceding
binary was deployed.

Both earlier failed batches remain in the evidence. The first omitted the
shared contract bundle and used an in-memory database in a durability test;
kernel rule validation also failed. The second corrected those inputs but still
rejected the kernel's extended-table compatibility header. The final reader
handles that header while retaining strict action and selector checks. Neither
failed batch was accepted for deployment.

## Actual Linux faults

Shipping restart, health, dependency, host PID/network, sysfs permissions and
shutdown settings were retained. Private overrides selected the checked binary,
isolated database, names, listener/authentication and exact CT secondary port.
Host IMS, VoWiFi, SIP, tasks and notifications were disabled in this data fixture.
Matching patched ModemManager was used during hardware faults; its existing
deployment qualification remains applicable.

| Active session killed | Automatic data plus public query | Public address family |
| --- | --- | --- |
| Dual stack | 51.790 s | IPv6 |
| IPv4 | 89.942 s | IPv4 |
| IPv6 | 64.393 s | IPv6 |

Each exact owner PID was signaled once through a verified PIDfd and exited 137.
Its existing container automatically restarted once. The old claim's durable
intent, empty-fence cleanup boundary, fresh owner PID/token/USB generation and
new ready receipt were observed. Each public query returned HTTP 200. There was
no manual restart, re-enable, modem reboot or fence deletion inside a fault
window. Normal disable/enable separated the independent family cases.

One complete 12-byte CFUN write was captured in each dual-stack and IPv4 case.
The IPv6 syscall observer did **not** capture that write. Its narrower evidence
is one durable intent, one submission log, original generation loss and the
fresh working connection; byte-level submission count is not established there.
IPv4 also retained transient primary AT probe/time-out failures before its
automatic recovery. No carrier call or SMS was placed; Philippines calls/SMS
were zero.

## Retained setup failures and IPv6 baseline maintenance

Read-only preflight refused a stale `ttyUSB3` mapping: the original CT secondary
interface had enumerated as `ttyUSB8`. Before deployment, the fixture switched
to its verified stable by-id alias, retaining exact physical/equipment/SIM checks.

Before the IPv6 fault, a ready IPv6 session failed two fresh public-IP queries
with DNS timeouts. Independent interface-bound probes to both supplied IPv6 DNS
servers and one public IPv6 DNS server also timed out. No owner fault had been
injected. A GET of the status view was not a successful reachability query.

The original live owner normally disabled data and released its claim. One
separate complete-module ADB maintenance reboot then re-established the baseline,
with native IMS kept at 1 and all three application/proxy processes retained.
This was module maintenance only; no ADB voice bridge was used. Initial helper
attempts aborted before the reboot intent; the supported listener syntax was
corrected and its loopback-only binding verified. Those failures were retained.
No DNS, route or Raw-IP repair was performed. A new explicit pre-fault enable
and fresh IPv6 HTTP-200 query succeeded before the subsequent owner SIGKILL.
The maintenance reboot is excluded from automatic recovery acceptance. General
IPv6 cold-start reliability is therefore still separately qualified.

## Restoration and remaining scope

Both application processes and the owned proxy stopped normally with exit 0.
The original equipment/SIMs were registered with RF on; native IMS 1 and every
PDP definition field matched the pre-test observations. The first restoration
observer failed a literal response comparison because the reboot enabled query
echo. A separate strict parser confirmed only the exact query echo differed,
with no unknown lines or duplicate contexts ignored. Raw responses and the
failed observer were retained; no extra hardware write was needed.

The original stock manager was restored and remained at the same PID with zero
automatic restarts for 121.070 seconds. Its known earlier distribution-manager
crash remains recorded in the [prior proxy report](VERIFICATION_20261028_COMPOSE_PROXY_ACTIVE.md);
this run does not fix that dependency. Original database hash, lock inode, five
running production container IDs, PH host state and CT host state except the
recreated interface index were preserved. Claims, private DNS, XFRM state,
owned override/executable and all three test containers were removed. No project
network or volume remained.

Legacy/unready/unknown-Start records retain their fence. Host boot/power loss,
physical hotplug, native systemd supervision cascades and carrier-bound SIP joint
faults require separate acceptance. Physical browser audio remains deferred at
the user's request. The overall hardware goal is still open.
