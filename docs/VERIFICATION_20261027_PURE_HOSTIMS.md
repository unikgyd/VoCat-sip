# Pure QDC507 Host IMS and SIM SMS provisioning — 2026-10-27

QDC507 follows the current VoWin **pure Host IMS** design. The former ADB/D4/UAC
voice bridge is deprecated and excluded from this target. This run corrected
that scope and fixed a real Host IMS SMS provisioning gap. Answered Host IMS
audio remains unverified.

The [machine-readable record](verification-20261027-pure-hostims.json) binds the
software checks and live result to the exact candidate. The source was frozen
after implementation, based on `17312c2`; its manifest hash is
`9fe924cac95046173357b24fe605417e6067ff30aaa2293d977dfffaff688d58`.

## Scope correction and retained observations

The preceding assisted calls used explicit module mode. The first captured
silence and the recipient heard no tones. A subsequent temporary ADB/UAC route
made tones audible with reported slight distortion. Neither call establishes
Host IMS carrier media. The preceding module SMS and recipient reply likewise
do not establish IMS SMS delivery.

The exact owned route/calibration processes were stopped, the original DTR bits
restored, and one controlled reboot cleared the volatile drivers and temporary
module files. Native IMS `1,1`, unchanged profiles and the host network baseline
were verified before setting up the separate pure Host IMS case. The private
ADB daemon was stopped after checking its PID, start time and executable hash.
No QDC507 ADB runtime was added to the product.

## Host IMS SMS failure and fix

The original CT equipment/SIM and USB topology were verified before maintenance.
The authorized baseband IMS handoff to mode `2` and a changed USB generation
were confirmed. The CT modem remained managed for its original IMS PDN; the
independent Philippines modem was inhibited. Tasks, external notifications,
SIP gateway, ordinary data and automatic Wi-Fi access stayed disabled in the
private database copy.

The previous checked candidate registered successfully and the web phone
reported `transport=host_ims`. Its first IMS SMS API request returned HTTP 502,
`ims_sms_submission_failed`, before any part was attempted. Source inspection
found that Host IMS supplied neither a SIM SMSC nor a service-centre reader.
The failed request was retained and not replayed.

The fix reads record 1 of the original USIM's EF_SMSP inside the existing owned
UIM logical-channel transaction. It validates the fixed parameter block, presence
indicator, address length/type and decimal BCD. The address accompanies the
provisioned IMS identity. The prepared access also exposes a fresh reader tied
to that same owner, with renewed access evidence and unchanged IMS identity.
Missing/invalid optional provisioning leaves voice registration available;
transport errors, uncertainty, cancellation and changed provisioning do not
produce an address. There is no carrier-number guess, raw AT or ADB fallback.

## Combined software checks

All changes were implemented before this combined run. Tests ran in private
mount/network namespaces with hidden hardware and separate runtime locks.

| Check | Result |
| --- | --- |
| Complete Go suite | 2,086 passed, 0 failed, 50 skipped; 190.256 s |
| Selected SIM/access race cases, five repetitions | 120 passed, 0 failed; 13.442 s |
| Isolated data-kernel cases | 3 passed; 1.318 s |
| `go vet ./...` | Exit 0 |
| Linux amd64 / arm64 / 386 / armv7 builds | All passed |

The deployed amd64 artifact is
`a0b31e7bf9457d32cb7054289c121c4905c5198f01282f42143b2d1b17f39031`.
Frontend code did not change; no new frontend test result is claimed.

## Live result on the checked candidate

The previous IMS owner acknowledged cleanup and its application exited 0 before
the new artifact started. The new candidate registered and advertised SMS on
attempt 1. Preflight checked the running executable, original equipment/SIM,
unique physical USB device, exact active claim, kernel security and explicit
`host_ims` cellular/default routing. It used no module audio route.

One new SMS request to the authorized recipient returned HTTP 202: one part
attempted, one part accepted, `transport=ims`, `accepted_by_ims`. The application
logged `smsc_source=sim`. The immediate response correctly left delivery
unconfirmed. An asynchronous IMS SMS status report then carried status code
**0**, and the original outgoing record's delivery state became **`delivered`**.
No recipient `OK` reply was observed before shutdown. Carrier delivery evidence
and a human-confirmed round trip remain distinct.

No call was placed in this pure Host IMS phase while waiting for recipient
availability. The paced test client was prepared but not dialed; it supplies no
answered-call, RTP/AMR quality, recording, hold or DTMF acceptance evidence.
The Philippines SIM submitted no SMS or calls.

## Harness observations and restoration

The setup's old legacy-refresh step returned HTTP 409
`host_ims_maintenance_required`. This is the intended ownership guard; setup
continued using verified ModemManager metadata without repeating firmware writes.
A later observer tried login before the replacement HTTP listener was ready and
received connection refused. It sent no enable or business request; the actual
enable followed confirmed readiness.

Both applications exited 0 after the original owner acknowledged PDN, network
and security cleanup. Claims and XFRM state/policy were empty. Native IMS was
restored to `1,1` through the authorized maintenance reboot; both original
equipment/SIM pairs were registered with RF on. All original PDP profile
definitions matched. Host rules, defaults, IPv6 settings, raw-IP mode, link flags
and MTU matched the baseline, with no global address left on the original data
interface. The distribution ModemManager service was restored and the exact
temporary executable/drop-in removed. The original database hash, server-lock
inode and running container IDs were preserved; the private database passed
integrity checking. The private ADB daemon remained stopped.

The full hardware objective remains open;
registration, carrier SMS delivery and module-mode audio cannot substitute for
answered pure Host IMS voice.
