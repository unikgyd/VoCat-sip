# Cellular data ownership and handover

2026-10-26 source checkpoint. These changes are **not yet tested or built**.
Historical verification snapshots do not verify this implementation.

Independent access must release the previous process-owned data session before
adopting another SIM, starting Host IMS or entering exclusive Wi-Fi access.
Changing `NetworkEnabled`, replacing a runtime status or canceling a request is
not a cleanup acknowledgement.

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

Remaining integration includes the complete task reservation/revert lifecycle,
all management and export-proxy paths, and final cancellation, failure, hotplug,
frontend and multi-architecture verification. Real firmware/carrier acceptance
remains separate from software verification.
