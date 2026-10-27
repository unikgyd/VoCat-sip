# Ordinary-data ownership fence

2026-10-27. Linux application integration for SIM-bound ordinary QMI data.

Before changing the data format or starting a packet call, the application reads
the equipment identity through the original QMI transport, verifies the original
SIM and commits a private ownership record using FULL-synchronous, exact-byte
SQLite compare-and-swap. The record contains hashed equipment, physical-device
and SIM identities, a random token, USB generation, interface, process ID and boot
ID. Subscriber identities and APN credentials are not stored in this record.

The live owner holds the same per-equipment process lock as Host IMS. A pending
ordinary-data record also denies a new Host IMS journal selection on that
equipment. Other known physical devices retain independent admission. Unknown
record headers fail closed. Records have no expiry, force replacement or public
API for deleting an ownership fence.

An uncertain Start retains the original client and record. A failed Stop retains
them until the original client confirms release or disconnection. Host cleanup
must also succeed before deleting the record. If packet release succeeds but
journal deletion fails, the next cleanup retries the journal without inventing
another WDS client or issuing a second stop. Confirmed physical disappearance
uses the existing original-generation cleanup boundary.

Cold startup checks pending records before data replacement, legacy RF writes
and automatic SIM power recovery. `StopOwnedNetwork` cannot acknowledge an orphan
just because the current process has no in-memory owner. An active owner in the
same process retains its metadata-only policy permission; the actual modem
operation still verifies its original SIM and physical generation.

This fence does **not** restore a WDS transport after process death. An unresolved
record remains pending and requires a separately acknowledged recovery path.
The optional [surviving data owner](DATA_OWNER.md) now supplies acknowledged
main-process cleanup while retaining the original transport and host capability.
Recovery after loss of that owner and a complete durable host-resource journal
remain open. Legacy unbound standalone controls keep
their previous contract. IPv4 cleanup's existing host-resource limitations are
not changed by this implementation.

Software and real-carrier results are recorded in
[the data-fence acceptance](VERIFICATION_20261027_DATA_FENCE.md).
