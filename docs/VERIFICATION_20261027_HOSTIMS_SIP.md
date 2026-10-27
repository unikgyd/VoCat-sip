# SIP extensions with pure Host IMS — 2026-10-27

A real Baresip extension placed one authorized China Telecom call through the
original QDC507's pure Host IMS session. The handset recipient heard the prompt,
spoke, and pressed repeated 5/8 keys. Bidirectional decoded audio, RTP and RTCP
were observed; negotiated RFC 4733 events for both digits reached Baresip.
An independent authenticated SIP MESSAGE produced one carrier-delivered SMS.
The recipient confirmed receipt and replied OK; the original SIM's inbox row
and durable SIP delivery plan matched that reply, with the authorized extension
returning SIP 200.

This first phase left incoming acceptance incomplete. The handset callback reached Host IMS
and rang both registered extensions, but the test answer observer exited before
submitting an answer. The recipient reported no answer. Later bounded observers
received no new callback. This is evidence of incoming delivery and ringing,
not successful SIP answering or incoming media. A later
[answered callback follow-up](VERIFICATION_20261027_HOSTIMS_SIP_INCOMING.md)
passed PCMA incoming answer/media and key 8; the initial failed attempt and its
JSON remain retained here.

The [machine-readable record](verification-20261027-hostims-sip.json) separates
these outcomes. The full hardware acceptance objective remains open.

## Candidate and actual endpoints

The unchanged checked amd64 artifact was
`5e9ab66648d0442fb4a0bb46c088c009f217cad8c08553b3adc4ac2432bf7044`.
All 1,774 selected implementation, test, frontend, script and module paths match
the frozen manifest. Previous software checks are
[referenced evidence](VERIFICATION_20261027_ESIM_TASKS.md), not new executions.
Only private test helpers changed during this phase.

Two actual Baresip processes registered on the trusted lab LAN: one offered PCMU,
the other PCMA. Only the original China Telecom equipment/SIM pair was authorized
for carrier calls and SMS. Original identities, USB generation, live Host IMS
ownership and explicit line permissions were checked before business submission.
The Philippines SIM made no calls and sent no SMS.

The available Baresip build rejected the initial SHA-256 Digest registration with
`Function not implemented`. Its idle private fixture was changed to MD5, after
which both clients registered. This records that client's compatibility limit;
it neither changes the application's default nor establishes SHA-256 Baresip
interoperability. Both endpoints used manual answering. The second endpoint had
no outbound carrier-call or SMS-send permission; only the first was an SMS
recipient.

Carrier audio used Host IMS AMR-NB and host PCM. QDC507 ADB/D4/UAC bridging,
AT voice dialing and host microphone/speaker devices were not used.

## Observed outcomes

| Scenario | Result and boundary |
| --- | --- |
| SIP extension to carrier | One dial submission; original Host IMS call and SIP leg active; PCMU ↔ PCM ↔ AMR |
| Recipient hearing and speech | Recipient confirmed hearing the prompt and speaking; endpoint decoded 3.436 seconds of received audio |
| Bidirectional media | Actual RTP in both directions; sampled SIP and carrier RTCP had zero reported loss and no invalid reports/send errors |
| Handset keys to SIP endpoint | SDP negotiated telephone-event/8000; actual PT 101 events and Baresip logs contain both 5 and 8 |
| Outgoing hangup | Original SIP and carrier calls both cleared after the one endpoint hangup |
| SIP SMS submission | One authenticated business MESSAGE; SIP 202; exactly one original-SIM outbound history row |
| Delivery and human reply | Carrier delivered; recipient confirmed receipt; original-SIM inbound OK matched the durable forwarding entry |
| Reply to SIP recipient | Only the authorized extension was in the captured recipient plan; its final SIP response was 200 |
| Carrier callback | Host IMS incoming event and both Baresip ringing legs observed; user reported unanswered; answer/media acceptance pending |

The 5/8 result proves that both digits crossed from the handset to the SIP
receiver. It does not establish completeness of the arbitrary key burst or
recognition by a remote carrier IVR in the opposite direction. The PCMA endpoint
registered and rang, but no answered PCMA carrier call passed in this phase.
The endpoints ran on the NAS; cross-network NAT/SBC and desktop browser audio
are outside this run.

## Retained observer failures

The outgoing observer initially required the carrier call to disappear in the
same read as the SIP leg. Carrier teardown was asynchronous. Later read-only
checks confirmed both original calls gone, without another dial or hangup.

The incoming observer encountered an unclassified HTTP failure, then retained a
`409 line_busy` response during a later read. Its preflight still had an immediate
empty-call assertion and exited before answering the actual callback. The helper
was repaired to use bounded read retries before arming. Subsequent windows
expired without a new call; no successful answer is inferred from that repair.
The original application was not restarted to repair an observer.

The first SMS observation window ended before the human reply. Continued
read-only observation found OK for the same submission; no SMS was resent.
The inbox timestamp was about 561.458 seconds after submission, with one-second
database timestamp resolution. The original 212.199-second observer duration is
retained separately and is not a completed round-trip latency.

A tentative log check matched registration's `200 OK`, rather than a MESSAGE
body. That check was rejected. Reply evidence comes from the exact inbox row,
reply text, durable recipient plan and final SIP response; endpoint body-display
logging is not claimed.

## Restoration and remaining work

Both owned SIP endpoints stopped, the application exited 0, and Host IMS claims
and XFRM state/policies were empty. The QDC507 returned to its original native
IMS setting 1 with registration confirmed. Both original equipment/SIM pairs
were registered with RF on. The stock ModemManager executable and service were
restored with zero automatic restarts; the temporary override/executable and
live owned inhibitors were absent. The original database hash, shared-lock
inode and pre-existing running-container IDs were preserved.

The restore guard initially paused after its already confirmed modem reboot
while comparing network state. Later read-only snapshots matched routing,
defaults and IPv6 settings. Restoration resumed at that checkpoint without
repeating the firmware write or reboot. Final China Telecom host state matches
the baseline except for the kernel interface index recreated by USB reappearance;
raw-IP mode Y, link flags, MTU and the remaining interface properties match.
The Philippines host network snapshot matches exactly. The JSON records these
specific comparisons rather than claiming an unchanged USB/interface generation.

Private credentials, subscriber identities, raw SIP/packet captures, database
copies, recordings and helper scripts stay outside Git.

SIP incoming answering/media was left open by this phase and subsequently passed
the linked callback follow-up. Actual browser
microphone/speaker use, remote IVR recognition, physical hotplug/SIM replacement,
independent data-owner/proxy loss, active-call/daemon/host failure, other firmware
and reader combinations, public release/clean-OS installation, disposable eSIM
provisioning and external notification delivery remain separate acceptance.
