# Deployed Linux acceptance follow-up — 2026-10-27

This continues the [first real-modem run](VERIFICATION_20261027_LIVE.md), from
source baseline `2340226`. The full Linux hardware goal remains open. A passing
subcheck below does not establish carrier voice, SMS delivery or every feature.

## Newly observed behavior

| Area | Actual evidence and boundary |
| --- | --- |
| Legacy modem management | With the EC20 serial port assigned to the dedicated test owner, live AT, APN, baseband IMS, operator selection and USB network-mode reads succeeded. Host IMS maintenance guards returned 409 on the other configured line. |
| Administration | The deployed service accepted authenticated operations, rejected a missing CSRF token and saved/read English and Chinese preferences, then restored the original preference. Browser login rendered the English dashboard; further UI checks await acceptance of the first-use license dialog. |
| SIP softphone media | Two real Baresip `1.1.0-3build3` clients used PCMU and PCMA. VoCat established both legs, transcoded in both directions and completed hangup. Decoded WAV captures contained the opposite endpoint's 440/880 Hz test tone, rather than merely increasing packet counters. |
| SIP media controls | Both endpoints received the other endpoint's RFC 4733 digit start/end events. Both media legs sent and received separate RTCP reports. These were internal extension calls, with carrier calling and SMS explicitly disabled. |
| LAN MESSAGE | An independent client on the Windows computer sent to the NAS and a real Baresip recipient confirmed delivery. A same-content business retry returned the original result without another delivery; changed content under that identity returned SIP 400. This did not send a mobile SMS. |
| Physical USB PCM | The original QDC507 USB audio pair opened at 8 kHz, mono, signed 16-bit. It captured 30 frames and played 30 frames, with zero dropped frames; the probe read 3,200 samples, wrote silence only and closed both original endpoints. No carrier call was placed by this probe. |
| Existing eSIM profile round trip | The final candidate enabled the other installed EC20 profile, verified its live identity, restored the original DITO profile and verified it again. Both switch responses returned HTTP 200. Both profiles had automatic access disabled for this controlled test; the inactive profile's original preferences were restored afterward. No profile was downloaded or deleted. |
| Message import | Real HTTP WAV, MP3, WMA and AAC uploads produced 8 kHz mono PCM previews. Stale revisions were rejected, imported messages were removed and the original disabled answering settings were restored. FFmpeg was installed in the dedicated test image. This is import/preview acceptance; live automatic answering, playback and takeover remain separate. |

The older Baresip/libre combination rejected the default SHA-256 challenge with
`Function not implemented`. Explicitly selecting the existing MD5 compatibility
option allowed that isolated listener to register both clients. The application
default was not changed, and this does not establish SHA-256 support in that
older client.
An independent client on the Windows computer also completed real LAN
registration and unregistration with the default SHA-256 challenge, both
returning 200. The legacy-client limitation did not require a global downgrade.

## Findings and repairs

- The runtime Docker image lacked ALSA even though a physical USB audio pair
  could be discovered. It now includes `libasound2`; the lab's media image
  demonstrated actual endpoint I/O.
- ModemManager and a legacy AT owner competing for one serial port caused
  timeouts. The controlled setup relinquishes only the selected legacy modem,
  preserving the ModemManager owner required by the other Host IMS line. See
  [deployment prerequisites](HOST_IMS_DEPLOYMENT.md).
- Two metadata requests could wait behind the AT owner past HTTP's write
  deadline and disconnect without a JSON result. Their operation admission now
  observes cancellation, and their read budgets leave time to send a structured
  timeout. A cellular IMS timeout is no longer mislabeled unsupported hardware.
- An eSIM EnableProfile could commit and verify the target ICCID while the
  complete ICCID/IMSI snapshot was still recovering. The first attempt switched
  the physical card but returned 409; the original DITO profile was restored
  and independently observed. Finalization now waits for the complete target
  identity on the same physical device without replaying EnableProfile. A
  metadata-only wait initially still failed because it held the line writer;
  finalization now runs a bounded guarded device refresh itself. Missing
  identities, changed USB generations and expired confirmation remain failures.

## Final checks and rollback

The final source passed **2,047 Go tests, 12 selected race-test executions**
(four top-level cases repeated three times), vet and Linux amd64/arm64 builds.
The Go run used `CGO_ENABLED=0` and retained 47 opt-in/environment skips. The
earlier frontend/kernel checks remain separate evidence; they were not counted
again in this run. One intermediate check failed to compile a new test that
compared a struct containing a slice; the corrected test and full rerun passed.

The final deployed amd64 binary SHA-256 is
`af97e7ec81f00dde9a0e373241ff592899914810fed5944fd49645d6786e2896`.
The [machine-readable record](verification-20261027-followup.json) includes
check results, both build hashes, actual media counters, eSIM responses,
import results and rollback evidence. The final candidate repeated the
softphone media/digit and LAN MESSAGE scenarios after the eSIM fix.

The application and both softphone containers exited with code 0. The original
active profile, inactive profile preferences and SIP configuration were restored.
Only the original test inhibitor was released; both original modem/SIM pairs
returned to registered service with RF on. The distribution ModemManager remained
running with zero restarts, original running containers retained their IDs, and
Host IMS claims, XFRM states and XFRM policies were all zero.

## Remaining acceptance

The authorized carrier call from the first run reached ringing without an
answer. Answered bidirectional carrier audio, incoming calls, carrier DTMF and
hold/resume, real call recording, automatic playback/browser takeover and mobile
SMS delivery remain unproven. VoWiFi carrier calls remain outside the user's
requested scope; connection acceptance was completed in the first run.

Physical hotplug/removal, crash/restart cleanup, data/export-proxy transfers,
scheduled jobs and external notification-provider delivery require their own
remaining live scenarios. Provisioning/download/delete needs disposable profile
material; it is not established by listing or switching existing profiles.

Private subscriber identities, eUICC identifiers, credentials, source packet
traces, database backups and generated audio remain outside Git. Intermediate
test-fixture failures and unsuccessful hardware attempts are retained separately
from the final acceptance evidence.
