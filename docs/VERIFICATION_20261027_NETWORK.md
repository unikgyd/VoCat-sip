# Deployed Linux data and shutdown acceptance — 2026-10-27

This continues the [deployed-service follow-up](VERIFICATION_20261027_FOLLOWUP.md)
from source baseline `4d42ee4`. The full hardware goal remains open.

## Real cellular data and independent clients

The original QDC507 and authorized China Telecom SIM established a process-owned
WDS data session using the card's existing `CTNET` APN. Automatic cellular and
Wi-Fi startup stayed disabled. The modem already reported non-voice CLCC
records; the test preserved them and did not force a radio transition or hangup.

An independent Python client on the Windows computer exercised authenticated
HTTP CONNECT and SOCKS5 proxies on the NAS. Both fetched a small HTTPS response
with normal TLS certificate verification and reported the same carrier public
exit. Missing/wrong HTTP credentials returned 407; wrong SOCKS credentials were
rejected. Header-only observation on the original modem interface confirmed
outgoing DNS and bidirectional TCP/443, with USB generation and netdev identity
pinned. Payloads and subscriber/public addresses remain private.

The original deployed candidate observed 70 transport headers, including 29
outbound and 25 inbound TCP/443 headers and eight DNS requests. This records
14,881 observed IP bytes, rather than a billing measurement or an exhaustive
traffic total. Each HTTPS response body was 226 bytes.

The data-off API refused to disconnect an enabled export proxy with HTTP 409.
Disabling only the two test proxies closed both held tunnels and stopped new
connections. A subsequent data-off request reached `disabled`, removed the
private DNS file and global modem-interface addresses, restored policy rules,
preserved the host's original main default routes and invalidated public-IP
cache detection.

This run exercises the IPv4 WDS export path. It does not establish IPv6-only or
simultaneous dual-stack data export, even though the saved card setting accepts
`IPV4V6`. Host IMS IPv6 acceptance is separate evidence.

## Clock dispatch and modem admission

A temporary `cellular_attach` task executed once by manual queue submission and
once by its actual scheduled clock time. Both attempts refused the module's
observed CLCC entries before changing the environment. Each had one attempt,
no retry, no notification and no call/SMS payload. The original device/card
configuration stayed unchanged, no recovery journal remained and only the
test task was removed.

This proves live dispatch and its admission guard. Successful task registration,
public-IP task execution and interrupted-task restoration remain unproven.
The later [voice-idle classification follow-up](VERIFICATION_20261027_CALL_IDLE.md)
identified those original entries as non-voice connections and corrected that
overbroad refusal. This historical refusal does not prove an active voice call.

## Shutdown repair

Review of the exit path found that device shutdown discarded the WDS client
without a coordinated data drain. The new process shutdown boundary fences new
requests, cancels and joins existing proxy/IP transfers and superseded data
workers, then invokes the original owned-session cleanup under each line gate.
Independent modems drain concurrently. Durable enable preferences are preserved
for the next startup, and incomplete cleanup becomes an application error.

The frozen candidate passed **2,051 Go tests**, with zero failures and 47
opt-in/environment skips, vet, **18 selected race-test executions** (six cases
repeated three times), and Linux amd64/arm64 builds. The general run used
`CGO_ENABLED=0`; race checks used `CGO_ENABLED=1`. Unchanged frontend/kernel
checks from earlier reports were not counted again.

The amd64 binary SHA-256 is
`d72ac3d601c74440f81c054b2d6faa1f1d73e4e322ed23162edbe06384828213`.
The [machine-readable record](verification-20261027-network.json) records
both build hashes, real client results, active shutdown and restoration.

The same final binary automatically restored saved data intent and both proxy
listeners after an orderly restart. The independent Windows client repeated
both authenticated HTTPS transfers (231-byte bodies), then held two tunnels
open while the application received SIGTERM. Both tunnels closed, new
connections were refused and the application exited with code 0. The pinned
interface observer finalized when the link went down: 90 transport headers,
36 outbound and 38 inbound TCP/443 headers, eight DNS requests and 23,704
observed IP bytes. Original policy rules/default routes were restored, owned
global addresses/private DNS were removed and saved enable intent remained.

After positively acknowledged data cleanup and application exit, the private
test database's original device/card preferences and proxy configuration were
restored. Restoring the original Host IMS selection used the stopped test
fixture, because the observed non-voice CLCC records prevent a live mode
transition. It is not counted as a successful live mode handover. Incoming
messages and SIP delivery ledgers were preserved. Only the two original test
inhibitors were released. Both original modem/SIM pairs returned to registered
service with RF on, and the original running Docker container IDs stayed
unchanged. Task recovery journals, Host IMS claims and XFRM state/policies were
zero.

The host's distribution ModemManager **segfaulted during inhibitor release**
and systemd automatically restarted it once. The original `/usr/sbin/ModemManager`
was running again when identities and registration were verified. This phase
does not satisfy a zero-restart ModemManager reliability requirement. The
private journal retains the crash; no core stack was available through
`coredumpctl` on this host. Its cause is not established by the successful Go
application shutdown.

The first combined software attempt omitted the sibling shared protocol
fixtures and failed 13 contract checks. That failed run is retained privately;
the fixtures were restored without changing the frozen application source.
Private test helpers initially had an incorrect USB parent-path assertion and
expected `disconnected` instead of the API's `disabled`. The first final-candidate
observer also failed to finalize when successful shutdown brought the netdev
down. Its original log and independent positive route/address/DNS cleanup audit
were retained; the corrected observer and independent client repeated the same
application binary. These fixture errors are not counted as product failures
or passing checks.

## Remaining boundaries

No carrier call or mobile SMS was submitted in this phase. The earlier ringing
call still does not prove answered two-way carrier audio. Incoming calls,
carrier hold/DTMF/recording/playback/browser takeover and SMS delivery still
need controlled endpoint participation. VoWiFi carrier calling remains outside
the requested test scope.

Orderly data shutdown does not establish abrupt process/machine crash recovery
or physical hotplug. Browser checks past the first-use agreement, external
notification-provider delivery and disposable eSIM provisioning/download/delete
also retain their separate acceptance requirements.
