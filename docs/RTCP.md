# RTCP call control and media statistics

2026-10-26 implementation checkpoint. This source has **not yet been tested or
built**. It is included in the final combined verification after implementation.

VoWin's reference is `voSharp/src/VoSharp.Telephony/Calls/RtpSession.cs`, including
its separate control socket, receiver reports, explicit remote RTCP endpoint and
protected bearer binding. The Linux implementation covers both IMS access
(`internal/vowifi/ims`) and SIP extension/peer media (`internal/sipgateway`).

Each media leg owns separate RTP and RTCP sockets. Offers and answers advertise
the actual control port with the media-level `a=rtcp` attribute. Remote SDP may
specify a non-adjacent port and a separate unicast address in the same family;
an absent attribute means RTP port plus one. Invalid/duplicate attributes,
unusable addresses and `rtcp-mux-only` are rejected. Optional `rtcp-mux` is not
accepted in the answer; this implementation uses the separate transport.

RTCP reception stays pinned to the negotiated control address and port. RTP's
symmetric source-port learning never retargets RTCP. Received sender-report
timing must match the accepted RTP SSRC; a single different RTP SSRC cannot
replace the current source. RTCP BYE updates diagnostics and does not hang up a
SIP dialog. Malformed compounds are validated in full before report state changes;
unknown bounded packet types are ignored. Input is bounded to 8 KiB and 32 RTCP
packets per datagram.

The shared `internal/rtcpmedia` implementation tracks RTP counts and octets,
extended receive sequence numbers, signed cumulative loss, interval loss,
interarrival jitter, LSR/DLSR and validated report round-trip samples. Accepted
late and duplicate RTP packets are included in reception accounting. Reports
use SR when recently sending RTP and RR otherwise; outbound reports include
SDES with a random per-call CNAME. No IMSI, ICCID, phone number or host name goes
into that identifier. Reporting uses a randomized unicast interval and attempts
a final compound report/BYE when closing. Failed sends do not advance the
successful reporting interval or cause another network path to be selected.

SDP commits update both remote endpoints together. An outdated control send
cannot borrow the newly configured endpoint or route. Host IMS binds the second
socket through the same verified bearer and exact assigned local source as RTP.
When RTCP uses a different remote address, its owned route is acquired before
commit. A failed acquisition leaves the previous negotiation in place and
retires any staged capability. There is no plain/default-network fallback.

IMS call termination queues RTCP cleanup without waiting inside SIP response
locks. Original bearer routes remain retained until the report sender, final
send and control receiver finish; explicit bearer teardown joins that cleanup.
Wi-Fi session teardown also drains its control sockets before the surrounding
tunnel is released. An exhausted cleanup budget closes the original control
socket immediately, preventing a late callback from sending after tunnel removal.
SIP gateway termination closes and joins both media socket paths. Audio and
telephone-event writes contribute to the same leg's RTCP accounting.

IMS call JSON exposes `media_quality`; gateway call status exposes
`caller_quality` and `callee_quality`. These are local observations, not a
guarantee of carrier delivery or audible quality. They contain no remote CNAME
or control-packet transcript.

Final verification must cover packet loss/wrap/reordering/duplicates and source
restart; SR/RR/SDES/BYE framing and malformed compounds; remote SSRC and endpoint
rejection; separate SDP ports/addresses/families; early media, hold/re-INVITE,
telephone events and PCM bridging; cancellation during the second route
acquisition; late sends and retained route cleanup; plus frontend and supported
Linux builds. Real carrier/firmware behavior remains a separate acceptance item.

Protocol references: [RTP/RTCP, RFC 3550](https://www.rfc-editor.org/rfc/rfc3550.html)
and [the SDP RTCP attribute, RFC 3605](https://www.rfc-editor.org/rfc/rfc3605.html).
