# Linux modem acceptance — 2026-10-27

This follow-up uses real modems and SIMs on the operator's Linux NAS. Dates in
the private logs are UTC on October 26; this report uses Asia/Taipei, October 27.
The earlier [software baseline](VERIFICATION_20261026.md) remains a separate run.

## Hardware results

| Check | Observed result |
| --- | --- |
| Host IMS | QDC507, firmware `QDC507GLEFM21`, China Telecom: real IMS IPv6 PDN, four P-CSCF addresses, UICC AKA, protected SIP registration and SMS capability advertisement passed. |
| VoWiFi | EC20-CE, firmware `EC20CEHHLGR08A04M1G`, DITO: authenticated ePDG tunnel, verified responder and IMS registration passed. No VoWiFi call was requested or placed. |
| Simultaneous operation | Both independent lines reported registered IMS sessions together. Stopping Host IMS preserved VoWiFi registration; stopping Wi-Fi preserved Host IMS registration. Both lines registered together again on the final build. |
| Authorized Host IMS call | One call to the operator's supplied test destination reached SIP `100 Trying` and `180 Ringing`, with AMR negotiated. It was canceled after about 27 seconds and ended with SIP `487`. |
| Voice media | The destination did not answer. No two-way speech or RTP audio delivery is claimed; the call counters reported zero RTP packets. |
| SMS | No SMS was sent. Advertising SMS capability is not an SMS delivery test. |

The registered/call build SHA-256 is
`320120d1c6c9875c1a914b1ac6795dfe6b09bdba378da4d85223a920a691b3d5`.
The subsequent cleanup fix build is
`9ff7a851144cd5270af20d8fefee453cab870ad48abf1bb46f665e4c3efc8ba1`.
Both were deployed through a dedicated temporary Docker container using this
fork's embedded frontend and the NAS's AMR runtime dependencies.

## Changes driven by the hardware run

- An exact private SIM allowlist admits the explicitly authorized test card
  before device discovery. No MCC-wide exception or real identity is committed.
- Per-card IP preference and an existing IMS profile can be selected. On this
  modem the original profile 5 supplies IMS P-CSCF discovery; the default data
  profile did not. No modem profile was rewritten.
- Acknowledged negative ModemManager Connect replies can clean up their exact
  created bearer. Timeout, cancellation and unknown-ACK fences remain intact.
- NAS parsing validates the observed extended LTE fields. Bound IPv6 WDS reads
  retain the original host IID only within the same proven /64 PDN; other
  identity, APN, gateway, mux and endpoint checks remain strict.
- A new owned multiplexed interface is brought UP under a journaled lifecycle.
  IPv6 RA/autoconfiguration is suppressed first and restored after cleanup.
- UIM uses explicit logical-channel opening and preserves the shared USIM
  application. A single expected channel-closed notification must match the
  original CID, slot and channel; it cannot replace a method ACK.
- Required socket ESP now resolves outbound SAs with the kernel's socket-policy
  mark semantics. Inbound modem marking remains in place. An outbound SPI
  collision preserves the original owner and blocks the new activation.
- Numeric TCP/UDP protocol selectors work in minimal container images without
  an `/etc/protocols` database.
- Failed and pre-registration idle lines can stop without bypassing active-call
  checks. Failed VoWiFi session closes retain their original cleanup references
  for acknowledgement before another activation.

The installed Ubuntu ModemManager `1.25.95-1ubuntu1` also exposed a netlink
completion use-after-free. A temporary matching-source build fixed the callback
lifetime; its SHA-256 is
`6b319c980e33256162ba68a4505807255eebe0f81dadde84f5b75979f61e41ac`.
The distribution binary was preserved. See the
[deployment notes and minimal patch](HOST_IMS_DEPLOYMENT.md).

## Verification and limits

The final checks passed: **2,043 Go tests, 52 selected race tests, 42 isolated
kernel tests, 79 frontend tests, vet, the frontend production build and Linux
amd64/arm64 builds**. The default Go run used `CGO_ENABLED=0` and skipped 47
opt-in/environment cases; the 42 kernel cases were then run explicitly without
skips. The machine-readable [verification record](verification-20261027-live.json)
contains the counts, kernel cases, image hashes and rollback summary. Linux kernel
tests use a fresh network and mount namespace with its own sysfs; they do not
operate on the real modem namespace.

Intermediate failures were investigated rather than counted as acceptance:
unsupported runtime/NAS fields, an incorrect APN profile, the distribution
daemon crash, a DOWN interface and unwanted RA route, expected UIM close events,
and socket-policy SA selection. Uncertain lab claims were archived and resolved
only after exact-owner hardware/kernel cleanup and, when needed, a selected-modem
reset. The application's normal uncertain-owner safeguards were not disabled.

One broad race run exhausted its five-minute limit without reporting a data
race. A later full Go run encountered a paired TCP/UDP fixture port collision;
the fixture now reserves the pair with bounded retry. The final record keeps
these initial outcomes separate from the final checks.

This run does not establish answered audio, USB PCM, real call recording or
playback/takeover, live SMS delivery, arbitrary firmware compatibility, long-term
stability or all deployment platforms. Private subscriber identities, SIP/AKA
material, keys, databases, phone numbers and raw hardware traces remain outside
the repository.

## Shutdown and rollback

The final build returned successful acknowledgements for Host IMS shutdown,
VoWiFi shutdown and repeated VoWiFi Stop. Synthetic failure/retry checks also
verify that a failed original session close cannot be replaced before its
cleanup is acknowledged.

The temporary container is stopped and both saved connection intents are off.
The test's modem inhibitors were released, and QDC507's original native IMS
mode was restored and read back as registered (`1,1`). The distribution
ModemManager binary is unchanged and running again; its temporary systemd
override was removed. The original modems/SIMs and running containers were
verified, with zero remaining Host IMS claims, XFRM states or XFRM policies.

Private lab evidence and both tested images are retained for reproduction.
