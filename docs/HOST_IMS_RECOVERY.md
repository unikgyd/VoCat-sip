# Host IMS restart cleanup

2026-10-26 implementation checkpoint. This new source is **not yet tested or
built**; it is part of the final combined verification.

A service-process restart does not prove that its IMS PDN, network objects or
XFRM/TC resources disappeared. The application now connects the private journal
to a cleanup-only recovery capability. It runs before preparing fresh Host IMS
access and when a stopped line still has a durable claim during a mode change.
The current line gate and device authorization remain in effect.
Startup and recovery use saved strong hardware anchors and live MM SIM metadata;
an old AT SIM snapshot cannot veto that lookup or silently select another card.
The inventory projection is copied rather than modifying shared discovery rows.

Recovery takes the equipment/process lock and checks the original daemon owner,
modem object, hardware/USB binding and SIM fingerprint. A confirmed network
record must also match its original bearer configuration, network namespace,
netdev index and sysfs anchor. Its private routing table is reused for reads and
deletes, never allocated again. Kernel objects are removed only through their
recorded keys and complete fingerprints. Borrowed addresses are not deleted.

Security recovery reopens the original reservation files and verifies their
tokens, permissions and process locks. It removes only the journaled XFRM/TC
objects, then the owned address/rule/routes, then disconnects and deletes the
exact original PDN. The claim clears with compare-and-swap only after cleanup.
The capability has no SIM authentication, socket, usable bearer or registration
surface. It never creates a PDN or new networking/security state to make an old
record look complete.

The factory roots one recovery owner per logical line. The owner and underlying
network/security workers survive a caller timeout. Repeated Stop/reconnect joins
or retries that same capability, and a late old closer cannot clear a successor
claim. Another modem keeps its own recovery and operation gates. Cleanup failure
keeps the shared backend alive for subsequent attempts.

Unknown bearer mutations, pending/uncertain SIM or runtime transactions, unknown
netlink ACKs, missing ownership hashes, changed objects, a changed service/SIM,
or missing security reservations remain fenced. There is no APN-based adoption,
modem reset, broad table flush or foreign-bearer deletion. A never-mutated
reservation can be cleared without touching the modem.

This recovers confirmed journals while the original daemon, bearer and kernel
identity are still provable. It does not infer ownership across an MM restart,
host reboot or replacement network namespace. A disconnected bearer whose
original network identity/configuration cannot be proved remains pending too.
Those conditions are explicit blocked states, not successful recovery. Real
firmware and restart acceptance remains separate from synthetic verification.

Final verification includes confirmed network/security cleanup, process locks,
absent claims, staged/unknown operations, token/identity/configuration changes,
foreign resources, caller cancellation and late worker completion, old-closer
replay, two-modem independence, and the application startup/Stop handover.
