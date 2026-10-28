# Host IMS deployment notes

Host IMS needs the host ModemManager, a shared QMI proxy, the real host network and
shared process/QMI lock directories. The compose definition mounts the system
bus and `/run/vocat` and `/run/lock`, and shares host PID and network namespaces.
Create `/run/vocat` as root with mode 0700 before starting a container. Run only
one VoCat deployment for each physical modem; its MM, proxy and owner services
cooperate within that deployment. Do not use an independent QMI client while
a line owns its bearer.

## Service ownership and ModemManager policy

For the current Host IMS deployment, retain one ModemManager backend and pin a
verified version. Its system D-Bus service supplies the modem/SIM identity and
creates, connects and releases IMS bearers. `vocat qmi-proxy` supervises the
shared transport, while `vocat data-owner` owns ordinary packet-data sessions;
neither replaces this IMS bearer backend. The self-managed WDS reader in Host
IMS reads the acquired bearer runtime and does not start an IMS bearer itself.

If the distribution ModemManager has the documented defect, select a verified
matching patched executable through the existing `ModemManager.service`
drop-in, following the dependency section below. Preserve the packaged binary
and unit hardening, keep only one daemon providing
`org.freedesktop.ModemManager1`, and retain the exact drop-in for rollback.
Replacing that executable keeps the required MM backend available. Globally
disabling or masking MM would remove the current Host IMS backend.

Before changing MM or proxy ownership, normally drain every affected user's
calls and data sessions and acknowledge their cleanup. Start the independently
managed proxy before the MM and VoCat clients. If another application already
owns a live proxy, coordinate its release instead of adopting or killing it.
For legacy AT-port conflicts, inhibit only the verified legacy device, as
described below; do not inhibit the modem currently providing Host IMS.

An environment using only ordinary data or VoWiFi can choose to stop MM after
confirming that no Host IMS line or other application needs it. Those paths do
not use the MM D-Bus bearer backend, but the recorded fault tests used a matching
patched MM; they do not establish acceptance of an MM-free deployment.

QDC507 follows the current VoWin Host IMS design: the host owns the IMS bearer,
authentication, SIP dialogs and RTP/AMR media. The former QDC507 ADB/D4/UAC voice
bridge is deprecated and is not a prerequisite or a fallback for this path.
Select cellular mode and default route `host_ims` explicitly; AT/USB calls or
module-submitted SMS cannot establish Host IMS carrier acceptance.

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

## Data runtime permissions and shutdown

IPv6 data export writes the selected interface's IPv6 settings; native QMI
format setup writes its `qmi/raw_ip` sysfs attribute. The systemd unit retains
`ProtectKernelTunables=true` with writable exceptions for
`/proc/sys/net/ipv6/conf`, `/sys/devices` and shared `/run/lock` reservations.
`/run/vocat` is a private runtime directory. The compose file keeps `/sys`
read-only and overlays `/sys/devices` writable for owned device setup.

Both shipping service definitions and compose allow 120 seconds for graceful
exit. HTTP, data transfers/WDS, carrier signaling and media/device owners have
separate bounded cleanup stages; a short supervisor grace period can otherwise
kill the process before those owners acknowledge release.

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

Place the verified temporary executable under `/usr/local/libexec` for this
evaluation. The distribution service's `ProtectHome=yes` hides build artifacts
under `/root`; pointing ExecStart there fails with systemd status 203. Preserve
the service hardening and distribution binary, and remove only the owned
temporary executable/drop-in after restoring the original service.
