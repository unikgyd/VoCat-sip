# Cellular data ownership and handover

2026-10-26 implementation. The combined software run is complete, including
transfer cancellation, original binding and stale public-IP results. See the
[verification results and hardware boundaries](VERIFICATION_20261026.md).

Independent access must release the previous process-owned data session before
adopting another SIM, starting Host IMS or entering exclusive Wi-Fi access.
Changing `NetworkEnabled`, replacing a runtime status or canceling a request is
not a cleanup acknowledgement.

The native QMI export path now implements IPv6 and dual-stack requests. IPv6
uses the modem's actual runtime address, prefix, gateway and DNS; it does not
try IPv4 DHCP for an IPv6-only connection. `IPV4V6` uses separate IPv4/IPv6 WDS
clients and retains both original handles. Both families must be ready before
dual-stack traffic is admitted; partial service is not silently downgraded.
An uncertain start or failed stop retains the relevant original client and
blocks replacement. Host cleanup preserves other interfaces' routes, removes
only the installed data addresses, and restores the original IPv6 settings,
MTU and link state for the IPv6/dual-stack owner. The
[combined software and real-carrier acceptance](VERIFICATION_20261027_IPV6.md)
records IPv6, dual-stack and IPv4 regression evidence and its remaining limits.

The runtime tracks all live workers, including workers whose status entry was
superseded. Handover fences new data requests, cancels and joins those workers,
then invokes `StopOwnedNetwork` directly under the line lifecycle fence. A worker
waiting for the line read gate observes cancellation, so cleanup does not queue
a disable job that would wait for the same writer to unlock itself.

The device capability retains the original USB generation, transport, WDS
packet-data handle and kernel netdev index. Cleanup uses that original client
and handle. It never opens another WDS client at a recycled control pathname or
calls `StopAny`. Confirmed physical disappearance releases the old transport
without flushing or bringing down the replacement netdev. Host cleanup removes
the original mark/rule and interface-specific routes rather than a whole table
belonging to another interface.

A Start timeout or missing handle is uncertain. Its original client is retained;
a confirmed disconnected status may prove cleanup. A still-connected session
without a returned handle remains `data_cleanup_pending` and needs recovery.
The new independent start path refuses unknown connected sessions instead of
reclaiming them with a modem-wide stop. Legacy unbound data controls retain
their existing explicit recovery behavior.

Independent data activation rechecks live ICCID/IMSI through the WDS client's
UIM surface before starting or reusing the session. AT data writes also verify
the original SIM; cleanup never deactivates a replacement card's CID 1. A
confirmed inactive CID or RF-off may prove that the original PDP ended.
Flight-mode changes serialize with data transitions and cancel queued native
NAS registration, preventing a previous background registration from undoing
the requested RF-off state.

Only after cleanup and target-policy persistence does the runtime admit new
data work. The target card supplies its own APN and saved data intent; Host IMS
and exclusive Wi-Fi do not inherit the previous card's data enable flag.
eSIM switching, task preparation and task restoration use this boundary.
Cold startup also detects a temporary telephony projection that differs from
the durable per-SIM preferences and reapplies the actual card's saved policy.

Task writer capabilities and original-call lifetime handling are now implemented
in [automatic tasks](AUTOMATIC_TASKS.md), with the same unverified status.
That path now includes a durable restore/revert journal and Host IMS task handover.

Export Proxy and public-IP lookup now acquire revocable user-plane leases on an
acknowledged data session. Merely observing an online interface does not grant
that ownership. Admission checks the current device/SIM policy, physical target,
ICCID, USB generation and kernel netdev index. Host IMS, exclusive Wi-Fi, a task
writer or pending task recovery cannot supply export traffic. A listener can
remain open while data is unavailable; the API and UI distinguish listening from
having an admitted data session. No admission callback means no outgoing traffic.

Data transitions, invalidation and handover cancel existing transfers. Workers
wait for transfer completion before invoking the next data operation; handover
also joins superseded transfers before releasing the original data owner. HTTP,
CONNECT, SOCKS dialing, DNS and established tunnels use the lease context.
Cancellation closes their original sockets, and both tunnel-copy directions
exit before returning the lease. Proxy disable, deletion, replacement and
developer-mode shutdown also close existing clients.

Public-IP cache entries carry the accepted data request revision as well as
ICCID. A result from a stopped or replaced request cannot populate a newer
session's cache. Wi-Fi coexistence does not suppress module data or IP lookup;
the call default route is independent. The Wi-Fi upstream-route reconnect
helper honors the line gate, current SIM and active Wi-Fi calls.

Public-IP detection races the provider's separate legacy IPv4 and IPv6
endpoints through the same owned session. The first valid response cancels the
other request, and both workers exit before returning the transfer lease. This
avoids exhausting an IPv6-only session's deadline on the IPv4 endpoint. Real
IPv4/IPv6 lookup and upgrade rollback are recorded in the
[native installation acceptance](VERIFICATION_20261027_NATIVE.md).

Graceful application shutdown now fences new data work, cancels and joins
existing proxy/IP transfers and superseded workers, then releases each original
data capability under its line gate before the device manager closes WDS.
Independent modems drain concurrently. Saved data-enable preferences remain
intact for the next startup; failed cleanup is returned as a shutdown error.
The fence also rejects late startup callbacks and task-finalizer admission.

This addresses orderly process exit. Abrupt termination, machine restart and
physical hotplug require separate live evidence; graceful cleanup does not
establish recovery of a lost WDS transport after a crash. Real firmware and
carrier acceptance remains separate from software verification.
