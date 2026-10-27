# Answered pure Host IMS callback to SIP — 2026-10-27

The authorized handset callback reached the original China Telecom QDC507's
pure Host IMS session and rang both real Baresip extensions. Extension 2 answered
once using PCMA; the carrier call used AMR-NB. The recipient confirmed hearing
the prompt, speaking and pressing 8. Actual received audio, negotiated RFC 4733
event 8, SIP/IMS answer confirmation and losing-leg cancellation were observed.
This completes the incoming answer/media scenario left open by the
[previous attempt](VERIFICATION_20261027_HOSTIMS_SIP.md).

The call ended when the winning test client's 40-second source reached EOF and
that client sent BYE. Both the SIP and original carrier call cleared. This proves
SIP-peer-originated cleanup; handset-initiated hangup is not established here.
The [machine-readable record](verification-20261027-hostims-sip-incoming.json)
preserves that distinction and the observer repairs. The full hardware objective
remains open.

## Candidate and observed outcomes

The unchanged checked artifact was
`5e9ab66648d0442fb4a0bb46c088c009f217cad8c08553b3adc4ac2432bf7044`.
All 1,774 selected implementation/test/frontend/script/module paths still match
the frozen manifest. No new software-suite or build execution is claimed.
Only private test helpers changed. Both clients used the previously established
MD5 compatibility fixture on the trusted NAS LAN; default SHA-256 client
interoperability is not inferred.

Original equipment, SIM identities, USB generation, live Host IMS claim and
authorized caller were checked before the single endpoint answer. QDC507 media
used host IMS/AMR/PCM, with no ADB, USB voice bridge or AT voice call. No outgoing
carrier call or SMS was submitted. The Philippines SIM made zero calls and sent
zero SMS.

| Check | Observed result |
| --- | --- |
| Incoming fork | One INVITE to each registered endpoint; both returned ringing |
| First answer | Endpoint 2 returned one INVITE 200; gateway ACK; original IMS answer confirmed |
| Losing endpoint | CANCEL, 200 for CANCEL, 487 for INVITE and ACK; no established media call |
| Media | PCMA ↔ host PCM ↔ carrier AMR-NB; RTP in both directions; human prompt/speech confirmed |
| Received speech | Actual Baresip decoded WAV: 8 kHz mono, 10.481 seconds of decoded samples, peak 10,496 and RMS 596.84 |
| Handset key | Telephone-event/8000 negotiated; ten captured event packets for 8, including three end packets; Baresip logged 8 |
| SIP-leg RTCP | Separate control ports; reports in both directions; sampled counters report zero loss, invalid reports and send errors |
| Teardown | Winning endpoint sent BYE at source EOF; gateway returned 200; both original calls cleared without an operator hangup command |

Captured PCMA contains 2,000 packets from the winning endpoint to the gateway
and 529 in the reverse direction. Decoding the captured receive payloads yields
10.58 seconds; the client WAV yields 10.481 seconds. Three of four inspected
one-second client blocks match exactly within that decode. Full byte equality
is false and the cause of the difference was not established. Neither lossless
recording nor a complete 40-second speech recording is claimed. Payload duration
is distinct from call duration and the observer's wait time.

The carrier-leg snapshot sent 11 RTCP reports and received zero reports during
this call. SIP-leg bidirectional RTCP evidence does not establish new
bidirectional carrier RTCP evidence.

## Retained observer findings

The first preparation attempt required the native call list to be empty. A
historical incoming object was positively terminated, with no active call or
bearer, but still made that observer time out. This happened before any
maintenance write. Its evidence was archived. The repaired preflight accepts
only positively terminated objects and continues to reject every non-terminated
call while checking the exact original equipment/SIM pair. No call-delete or
hangup command was used to bypass that check.

The answer observer retained one transient HTTP `line_busy` read and recovered
with bounded reads. It did not restart the application or submit another answer.
An initial packet analyzer also misclassified RTCP as RTP; compound RTCP framing
was separated before publication without repeating carrier business.

The first result named call disappearance as handset hangup. The actual endpoint
log and SIP dialog showed source EOF and endpoint-originated BYE, so that label
was corrected. The initial observer result remains private rather than being
treated as proof of the handset's hangup path.

## Restoration and remaining acceptance

Both owned clients stopped, the application exited 0, and Host IMS claims and
XFRM state/policies were empty. The original native IMS setting 1 and registration
were confirmed; both original equipment/SIM pairs were registered with RF on.
Stock ModemManager was restored with zero automatic restarts, and its temporary
override/executable and live owned inhibitors were absent.

China Telecom host network state matches the baseline except for the interface
index recreated by the authorized modem reboot; raw-IP Y and other interface
properties match. The Philippines host snapshot matches exactly. Original
database hash, lock inode and running-container IDs were preserved, and no new
outbound SMS history row appeared. Raw packets, audio, credentials, subscriber
identities, databases and helpers stay outside Git.

Remaining scenarios include handset-initiated SIP bridge hangup, actual browser
microphone/speaker, remote IVR recognition, physical hotplug/SIM replacement,
independent data-owner/proxy loss, active-call/daemon/host failure, additional
firmware/readers, public release/clean-OS installation, disposable eSIM
provisioning and external notification providers.
