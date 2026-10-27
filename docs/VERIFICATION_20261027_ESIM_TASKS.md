# eSIM task reversion and restart recovery — 2026-10-27

The original EC20/eUICC completed a manually queued registration task using the
other installed profile, then reverted to the original profile and preferences.
A separate real SIGKILL after target-profile publication restarted the same
container automatically and restored the original environment. The interrupted
run stayed failed; its business action was not replayed. No call or SMS was
requested, and no profile was downloaded or deleted.

The [machine-readable record](verification-20261027-esim-tasks.json) binds each
failure and result to its candidate. The full acceptance objective remains open.

## Live failures and fixes

The first task used the preceding checked artifact
`ef2ebfced99cf736e21f890adbaf386a7eee8f8677a6a9925727843bb1ed4ca0`.
It ran once and failed during profile restoration. The original recovery journal
and line fence remained intact. A private UART observation showed that the
stored AID was selected and returned `6A82`, while the standard management
application remained accessible.

The task had saved the original **profile's ISD-P AID** where the switch transport
requires the containing **storage's ISD-R management AID**. Preparation now
resolves the unique storage containing the original ICCID from live inventory
and records its management AID/EID. Reversion verifies original storage EID and
profile membership again. Old version-1 journals remain usable: their original
storage is resolved from that live evidence, rather than trusting the erroneous
stored profile AID. Missing profiles, changed EIDs, conflicting metadata or
ambiguous storage retain the fence before reversion RF/profile writes.

The first combined software run also found a new fixture passing a device
configuration before database type normalization. It stopped at the hardware
fence before reaching the intended inventory check. The fixture now loads the
stored configuration and asserts that the inventory path was actually reached.
Its failed run is retained separately.

The AID-only v2 candidate passed its software checks, but a controlled upgrade
left the original recovery pending. The discovered PH modem had no in-memory
IMEI/SIM snapshot. Ordinary startup refresh was correctly fenced by the journal,
while restoration required that very snapshot before performing a refresh.
The selected UART had no read/write calls during a 35-second observation.
The observation timed out; the application remained alive and fenced, without
another task submission.

Module restoration now handles that empty-cache case with a bounded, read-only
`AT+CGSN` observation on the journal's already-bound original USB/serial/reader
evidence. The observed IMEI and USB generation must match before access/data
cleanup. The following full refresh must publish the original hardware identity;
the existing original/target SIM, idle-call, eUICC and cleanup checks still apply.
An unknown IMEI, changed generation or absent stable binding is refused before
eUICC access. Host IMS keeps its metadata-only path and performs no such legacy
AT bootstrap.

The initial private fixture also inherited a module-mode CT projection while
ModemManager owned that serial port. Before v3 rollout, only the stopped private
copy was corrected to inert Host IMS metadata mode. The PH recovery journal and
original database were preserved. A separate private-harness field-case error
(`iccid` versus the journal's `ICCID`) stopped before its upgrade intent or any
deployment mutation. These setup observations are not passing carrier results.

## Final software candidate

The final candidate is based on `2871642`. Its 1,896-file source manifest is
`1827cd13aeae00541db75fe7d8093ea719d94ce68fafea6d61e00f8db4e0c36b`.
Implementation preceded the combined checks; testing used private mount/network
namespaces with hidden hardware and separate runtime locks. Go 1.27.3
used local toolchain selection and offline, read-only module resolution.

| Check | Final result |
| --- | --- |
| Complete Go suite | 2,096 passed; 0 failed; 50 skipped; 197.238 s |
| Original-storage/cold-recovery/Host IMS race cases, five repetitions | 15 passed; 0 failed; 169.257 s |
| Isolated data-kernel cases | 3 passed; 1.320 s |
| `go vet ./...` | Exit 0 |
| Linux amd64 / arm64 / 386 / armv7 | All built successfully |

Final amd64 SHA-256:
`5e9ab66648d0442fb4a0bb46c088c009f217cad8c08553b3adc4ac2432bf7044`.

All 1,774 selected implementation/test/frontend/script/module paths still match
the frozen source. The 868 frontend paths remain byte-identical to their
[preceding 82-case run](VERIFICATION_20261027_HOSTIMS_CALL_TASKS.md); no new
frontend execution is claimed. The preceding v2 candidate separately passed
2,096 Go cases and 50 selected race executions. Those results are not attributed
to the final candidate. The first v1 run recorded 2,095 passes, one fixture
failure and 50 skips.

## Real task and recovery outcomes

The original equipment, subscription, physical USB device and installed profile
pair were verified before submission. Tasks selected the explicit EC20 line,
`cellular_attach`, `environment=cellular`, an empty payload, reversion enabled
and zero business retries. Automatic clock dispatch stayed disabled for these
manually queued cases. A matching patched ModemManager was used during the
window, with the original service restored afterward.

| Case | Observed result |
| --- | --- |
| Original failed journal, controlled v3 upgrade | Original profile/preferences restored; journal cleared; no new task/enable/switch API request; 84.249 s |
| Fresh profile registration task | One successful attempt; target profile observed; original profile/preferences restored; journal cleared; verified in 160.061 s |
| Separate post-switch SIGKILL | Original container emitted `die` with exit 137 and `start`; new main PID; restart count +1; interrupted run failed once; original environment verified in 84.737 s after kill |
| Concurrent same-line control | HTTP 409 `line_busy` in 0.002 s |
| Other original line read | HTTP 200 in 0.008 s; inactive Host IMS metadata configuration |

The SIGKILL checkpoint followed confirmed target-profile publication and was
during registration preparation, potentially with registration I/O in flight.
It did not interrupt the EnableProfile APDU itself. No manual restart or new
enable/task submission followed SIGKILL. The recovery case passed by restoring
the environment while retaining a failed interrupted run, not by relabeling that
run as successful.

Both fresh cases preserved all durable telephony/card preferences, original
eUICC EID, ModemManager PID/restart count and both USB generations. Outbound SMS
counts stayed unchanged. Failed and successful runs were exported privately
before removing only the owned tasks; normal task deletion cascades their live
run rows. The original failure was not rewritten into a passing result.

## Final restoration and limits

All three phase applications exited 0. The final audit confirmed both original
SIMs registered with RF on, original installed profiles/states/EID, unchanged CT
native IMS `1,1` and PDP definitions, original link/MTU/addresses/rules/defaults/
IPv6 settings, and empty IMS/XFRM/task recovery state. The original ModemManager
binary was selected with zero automatic restarts of the restored service; owned
temporary manager files were removed. Original database hash, server-lock inode
and running-container IDs were preserved. No maintenance modem reboot was
requested. The Philippines SIM made zero calls and sent zero SMS.

A later [clock and EnableProfile boundary follow-up](VERIFICATION_20261027_ESIM_BOUNDARIES.md)
adds separately scoped real clock dispatch and request/response-boundary recovery.
The cases in this report do not establish real clock dispatch for profile-switch
tasks, interruption inside an LPA commit, disposable-profile provisioning, another eUICC/reader,
active-call/daemon/host crash recovery, physical hotplug or independent data-owner
loss. Actual browser devices, carrier SIP interoperability and external-provider
delivery also remain separate acceptance. The distribution ModemManager UAF
limitation is not repaired by these cases. Private identities, profile names,
credentials, destinations, UART traces and payloads remain outside Git.
