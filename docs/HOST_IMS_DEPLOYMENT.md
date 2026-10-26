# Host IMS deployment notes

Host IMS needs the host ModemManager, its QMI proxy, the real host network and
shared process/QMI lock directories. The compose definition mounts the system
bus and `/run/vocat` and `/run/lock`, and shares host PID and network namespaces.
Create `/run/vocat` as root with mode 0700 before starting a container. Do not run
two services against the same hardware or use an independent QMI client while
a line owns its bearer.

Legacy AT operation also needs a single serial-port owner. In the NAS lab,
ModemManager competing for the EC20 AT port caused command timeouts. A dedicated
`mmcli --inhibit-device=<verified device UID>` process temporarily released only
that legacy modem; its process lifetime was retained and acknowledged. The QDC507
remained managed by ModemManager for Host IMS. Stop and clean up VoCat before
releasing the exact inhibitor, then verify the original SIM and radio state.
Do not stop ModemManager globally when another line needs its Host IMS bearer.

Native USB PCM needs `libasound.so.2` (Debian/Ubuntu `libasound2`). The Docker
runtime now includes this dependency. Pairing metadata alone does not establish
that ALSA can open the endpoints or that a carrier call has audio. Compressed
message import separately needs optional FFmpeg; WAV import does not.

## Existing IMS profiles

In the cellular line's advanced settings, `0` selects the modem default.
An explicit `host_ims_profile_id` selects an existing 3GPP profile (1–255).
VoCat does not create, edit or delete modem profiles. Use the carrier's existing
IMS profile with P-CSCF discovery enabled; an APN string alone may still select
a default data profile without P-CSCF information. Changing the profile stops
the original cellular owner and requires acknowledged cleanup before restart.

The card's IP preference controls the request when selecting automatically.
An explicit profile can dictate its own IP family. IPv6-only service on a
dual-stack IMS profile is accepted only when ModemManager reports a usable,
owned address and the bound WDS runtime agrees.

## Baseband IMS handoff

Some firmware prevents a host IMS bearer while the baseband IMS stack owns its
native IMS connection. On the tested QDC507 firmware, a controlled
`AT+QCFG="ims",2` followed by a modem reboot was required. VoCat's bearer
constructor does not issue this command or reboot hardware automatically.
Perform a firmware-specific handoff only in an idle maintenance window, record
the original setting and SIM/equipment identity, and restore the original
setting with another modem reboot after testing. Do not apply this sequence
to an arbitrary modem or use it during a call.

The host prepares only its proven multiplexed interface. For a new DOWN link,
it journals and disables IPv6 RA/autoconfiguration before bringing it UP, then
adds the exact address, source rules and P-CSCF routes. Cleanup removes owned
resources, restores DOWN and the original IPv6 settings. It does not change the
lower shared interface or the host's default route.

Linux socket ESP policies resolve outbound SAs with policy mark zero. Outbound
SAs therefore use mark zero and the socket's exact negotiated endpoint/ReqID;
inbound SAs retain the private modem mark and ingress classifier. The journal
records this mode so older marked journals remain cleanup-only compatible.
Two registrations sharing a P-CSCF must have distinct outbound SPIs. An outbound
SPI collision stops the new activation and preserves the original owner.
Required socket ESP policies continue to prevent plaintext fallback.

## ModemManager 1.25.95 dependency

The lab's Ubuntu `1.25.95-1ubuntu1` package crashed during netlink transaction
completion. The transaction hash removal frees `tr`, but the success path then
dereferences `tr->completion_fn`. A temporary build saved the callback before
removal; the [minimal source patch](patches/modemmanager-1.25.95-netlink-completion.patch)
records that exact local dependency fix. It is not an automatic package updater
or evidence of compatibility with other releases.

For controlled evaluation, build against the matching distribution source and
dependencies. Keep the distribution binary untouched, select the temporary
binary with a runtime systemd drop-in, and remove that exact drop-in when
rolling back. Stop and clean up VoCat's owned bearers before restarting
ModemManager. Preserve unresolved ownership journals until hardware and kernel
cleanup have been independently proven; do not delete them to force a retry.
