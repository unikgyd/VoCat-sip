# SIP handset hangup and retained media cleanup — 2026-10-27

One new authorized handset callback reached two real Baresip endpoints through
the original CT pure Host IMS line. Endpoint 2 answered with PCMA, the carrier
used AMR, and answer/ACK, bidirectional media, handset digit 8 and cancellation of
the other ringing leg were captured. The human confirmed actively hanging up
the handset. The gateway then sent BYE to the winning endpoint and received 200;
no endpoint-originated BYE preceded that termination. The answered carrier
interval was 16.363 seconds. Hearing was not separately confirmed in this phase.

This used main revision `165b50d3eb9f72eca4c0d7761c4a86d6c010283f` and the unchanged
checked amd64 artifact
`cf7c11fedba7aa58435180fda225fd44692d5294a22956f55e14171deccfdc20`.
The [sanitized initial record](verification-20261027-sip-lifecycle-initial.json)
retains packet counts, recording checks, failure boundaries and restoration.

## Partial success and actual failure

SIP active calls disappeared, the carrier call became ended, and its stereo
recording completed with zero dropped samples. The 300-second endpoint source
had not reached playback exhaustion during this call. An `aufile` EOF log while
loading the source is not evidence that playback ended; packet direction and the
human hangup distinguish this case from the earlier 40-second source termination.

**Complete per-call resource cleanup did not pass.** One original-owned media
route remained in the Host IMS journal, and the ended call stayed retained across
more than 74 seconds of terminal observations. The observer timed out because it
required an empty carrier history; its final endpoint hangup returned “no active
call” and performed no signaling action. Neither that observer assumption nor
successful SIP BYE is used to hide the retained route. The initial cleanup
rejection's exact cause was not instrumented and is not assigned a failure class.

The SIP endpoints were stopped and the gateway disabled. Stopping the same
original Host IMS session then acknowledged media/network/security/PDN cleanup,
released the claim and exited the application with code 0. A bounded original-
capability cleanup retry and a new frozen combined software run are the next
implementation step; repaired hardware behavior still needs a separate live run.

## Browser deferral and restoration

Before this callback, Chrome reached the authenticated localhost web-phone page
with the CT pure Host IMS line ready. Microphone preparation returned `Requested
device not found`, so no browser carrier dial was submitted. The human requested
skipping physical web audio because the computer was being used remotely and SIP
is the primary workflow. Browser microphone/speaker acceptance remains deferred.
The owned browser tab and SSH localhost forward were closed.

Native IMS `1,1`, original PDP definitions and both original equipment/SIM pairs
were restored and registered with RF on. CT host state matched except for the
interface index after the authorized module maintenance; PH host state matched
exactly. The stock manager returned with zero automatic restarts, and its owned
override/executable were removed. Original database hash, shared lock inode and
running container IDs were preserved, with no Host IMS claim or XFRM state/policy
left. This phase submitted no outgoing carrier call or SMS; the Philippines SIM
had zero calls and zero SMS.
