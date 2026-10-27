<p align="center">
  <img src="web/public/favicon.svg" width="96" alt="Vocat">
</p>

<h1 align="center">VoCat</h1>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.25-00ADD8?style=flat-square&logo=go&logoColor=white">
  <img alt="React" src="https://img.shields.io/badge/React-19-61DAFB?style=flat-square&logo=react&logoColor=111111">
  <img alt="TypeScript" src="https://img.shields.io/badge/TypeScript-5.8-3178C6?style=flat-square&logo=typescript&logoColor=white">
  <img alt="Vite" src="https://img.shields.io/badge/Vite-7-646CFF?style=flat-square&logo=vite&logoColor=white">
  <img alt="Tailwind CSS" src="https://img.shields.io/badge/Tailwind_CSS-3-06B6D4?style=flat-square&logo=tailwindcss&logoColor=white">
  <img alt="SQLite" src="https://img.shields.io/badge/SQLite-Embedded-003B57?style=flat-square&logo=sqlite&logoColor=white">
</p>

<p align="center">
  <img alt="Linux" src="https://img.shields.io/badge/Linux-amd64_%7C_386_%7C_arm64_%7C_aarch64_%7C_armv7-FCC624?style=flat-square&logo=linux&logoColor=111111">
  <img alt="Docker" src="https://img.shields.io/badge/Docker-Multi--Arch-2496ED?style=flat-square&logo=docker&logoColor=white">
  <img alt="WiFi Calling" src="https://img.shields.io/badge/WiFi_Calling-IMS_SMS-7B1FA2?style=flat-square">
  <img alt="eSIM" src="https://img.shields.io/badge/eSIM-LPA_%2F_eUICC-009688?style=flat-square">
  <img alt="Telegram" src="https://img.shields.io/badge/Telegram-Bot-26A5E4?style=flat-square&logo=telegram&logoColor=white">
  <img alt="GitHub Actions" src="https://img.shields.io/badge/GitHub_Actions-Release-2088FF?style=flat-square&logo=githubactions&logoColor=white">
</p>

**English** | [简体中文](docs/README.zh-CN.md) | [العربية](docs/README.ar.md) | [繁體中文](docs/README.zh-TW.md) | [Français](docs/README.fr.md) | [Русский](docs/README.ru.md) | [Español](docs/README.es.md) | [日本語](docs/README.ja.md)

Vocat is an open-source web control panel and engineering toolkit for Quectel EC20/EC25-class and China Mobile ML307 series cellular modems. It combines modem discovery, live radio status, AT and USSD terminals, SMS, WiFi Calling, eSIM management, network selection, proxy routing, notifications, audit logs, and release automation in one self-contained service.

This fork is the Linux client alongside [VoWin](https://github.com/unikgyd/VoWin).
The goal is the union of both clients' capabilities, with most implementation work
in VoCat-sip. The original [research and evaluation license](LICENSE) and runtime
authorization checks remain in force.

## Development status — 2026-10-27

| Area | Current source status |
| --- | --- |
| Calls and media | Web phone, physically paired USB PCM, SIP extension/peer bridges and negotiated RFC 4733 DTMF are wired. |
| Multiple modems | Independent runtime ownership, Host IMS, per-SIM ePDG history and persistent IKE retry admission are present. |
| Independent access | Per-ICCID cellular/Wi-Fi preferences, default routing, session ownership, reconnect controls and UI are implemented. Combined software tests and builds passed. |
| Management | eSIM, USSD, network/proxy tools, scheduled tasks, SMS API and notification channels are retained. Per-SIM startup, live Host IMS identity, owned data/transfer cleanup, durable task restoration, Host IMS handover and coexistence-aware proxy/data controls are integrated. |
| Advanced calls | IMS hold/resume, legacy SIP INFO, per-SIM delayed answering, imported playback, browser takeover, recording and separate RTCP/statistics are integrated. See the verification record for the tested software scope. |
| Live Linux acceptance | QDC507/China Telecom Host IMS and EC20/DITO VoWiFi registered simultaneously on the NAS. The authorized Host IMS call reached ringing and negotiated AMR; no answer was received. |
| Further live acceptance | Real Baresip extension media/transcoding, RTCP and DTMF, LAN MESSAGE confirmation/deduplication, physical USB PCM I/O, an existing eSIM profile round trip and four prompt import formats passed their scoped checks. |
| Cellular data export | Real CTNET IPv6, simultaneous IPv4/IPv6 and IPv4 HTTPS through authenticated HTTP/SOCKS5, IPv4/IPv6 public-IP queries and original data cleanup passed. |
| Native installation | Scoped real systemd installation, failed-upgrade rollback, original administrator preservation and shared-lock retention passed. |
| Remaining acceptance | Answered carrier audio, USB call media, live playback/takeover, carrier softphone bridging and other hardware/installation combinations. |

The initial combined software run passed: **2,025 native Go tests, 2,020 static Go
tests, 79 frontend tests, 132 selected race-test executions, 41 isolated kernel
tests, vet and four Linux builds**. Skip counts, retained failures and hardware
limits are recorded in [the verification report](docs/VERIFICATION_20261026.md).
Implementation preceded testing as requested. The subsequent hardware findings,
fixes, call result, cleanup and rollback are recorded in the
[live Linux verification report](docs/VERIFICATION_20261027_LIVE.md).
The [deployed-service follow-up](docs/VERIFICATION_20261027_FOLLOWUP.md) records
the next acceptance scenarios, observed failures and remaining hardware work.
The [IPv6 and dual-stack follow-up](docs/VERIFICATION_20261027_IPV6.md) records
the latest candidate's 2,057 Go checks, race/kernel checks, amd64/arm64 builds,
real data transfers, dependency requirements and shipping-permission evidence.
The [native installation follow-up](docs/VERIFICATION_20261027_NATIVE.md) records
the subsequent 2,062 Go checks, real systemd rollback and IPv4/IPv6 public-IP repair.
The [isolated Docker crash follow-up](docs/VERIFICATION_20261027_CRASH.md) records
real SIGKILL and saved-intent connection restoration on the original EC20/DITO
SIM, with host-network crash recovery still listed separately.
The [QMI ownership inquiry](docs/VERIFICATION_20261027_QMI_OWNERSHIP.md) records
the fresh-client status observations that constrain ordinary-data recovery.
The [ordinary-data ownership fence](docs/DATA_OWNERSHIP_FENCE.md) records ownership
before a SIM-bound QMI start and prevents a new process from acknowledging or
replacing an unresolved owner. With the optional [surviving owner](docs/DATA_OWNER.md),
real host-network main-process SIGKILL and automatic IPv4/IPv6/dual-stack recovery
passed the [new acceptance cases](docs/VERIFICATION_20261027_DATA_OWNER.md).
Loss of the owner itself remains separate acceptance.

The backend uses Go, React/TypeScript is embedded in the production binary, and
SQLite stores configuration and runtime history. See the
[feature union checklist](docs/FEATURE_UNION.md),
[independent-access design and remaining work](docs/INDEPENDENT_ACCESS.md), and
[repository conventions](docs/REPOSITORY.md).

<p align="center">
  <img src="img/image.png" alt="VoCat device overview">
  <img src="img/image-1.png" alt="VoCat management interface">
</p>

## Features

### Linux integration documentation

- [Host IMS application](docs/HOST_IMS_APPLICATION.md) and [ownership boundaries](docs/HOST_IMS.md).
- [Explicit test SIM policy](docs/AUTHORIZED_TEST_SIMS.md) for a privately configured authorized subscriber.
- [Host IMS deployment](docs/HOST_IMS_DEPLOYMENT.md), including existing IMS profiles, firmware handoff and the lab's ModemManager dependency fix.
- [Host IMS restart cleanup](docs/HOST_IMS_RECOVERY.md), including confirmed journals and unresolved ownership fences.
- [Final acceptance ledger](docs/PARITY_ACCEPTANCE.md), including the required evidence and remaining hardware acceptance.
- [Cellular data ownership and handover](docs/CELLULAR_DATA_HANDOVER.md).
- [Automatic task ownership and restoration](docs/AUTOMATIC_TASKS.md).
- [Web phone](docs/WEB_PHONE.md), [USB module voice](docs/MODULE_VOICE.md), [AMR media](docs/AMR_MEDIA.md), and [DTMF](docs/SIP_DTMF.md).
- [IMS hold/resume](docs/IMS_CALL_HOLD.md), including transaction and media ownership boundaries.
- [RTCP and media statistics](docs/RTCP.md), including separate control endpoints and bearer cleanup.
- [Automatic answering and message playback](docs/AUTO_ANSWER.md), including upload formats, timing and browser takeover.
- [Call recordings](docs/CALL_RECORDINGS.md), including stereo PCM, retention, private storage and interrupted-file recovery.
- [SIP gateway](docs/SIP_GATEWAY.md), [carrier calls](docs/CARRIER_OUTGOING.md), [incoming calls](docs/CARRIER_INCOMING.md), and [SMS bridge](docs/CARRIER_SMS.md).
- [SMS query API](docs/SMS_API.md) and [ePDG discovery](docs/EPDG_DISCOVERY.md).

Software wiring and synthetic or loopback evidence are distinct from hardware,
softphone, carrier and installation acceptance. See the feature checklist above
for the current boundaries.

| Area | What Vocat provides |
| --- | --- |
| Device management | Automatic serial/USB discovery, multiple modem support, friendly device names, live overview updates, module restart, flight mode, and USB networking mode controls. |
| Radio and network | Registration status, operator, signal metrics, RSRP/RSRQ/SINR, network mode, band, channel, operator scanning, automatic or manual network selection, and LTE cell querying and single-cell lock/unlock (EARFCN/PCI). |
| AT and USSD | Interactive AT terminal, command history, raw modem responses, USSD start/continue/cancel flows, and clear modem error reporting. |
| SMS | Direct cellular and IMS SMS transmission, inbound synchronization, multipart handling, delivery reports, conversation history, unread state, timestamps, and per-message delivery status. |
| WiFi Calling | IKEv2/ePDG tunnel setup, EAP-AKA authentication, IMS registration, IMS SMS, reconnect controls, status diagnostics, and per-device routing. |
| eSIM and eUICC | eUICC discovery, EID and production information, certificate metadata, multi-eUICC inventory, installed profile listing, enable/disable/switch operations, download, rename, and delete operations when supported by the card. |
| Card policy | ICCID-based WiFi Calling and flight-mode behavior with immediate policy application. |
| Proxy routing | Upstream SOCKS routing, device bindings, country rules, TCP reachability checks, and UDP Associate checks for WiFi Calling data paths. |
| Notifications | New inbound SMS forwarding through Telegram, Bark, email, Pushplus, and signed webhooks. Each SMS is delivered as an individual notification. |
| Telegram bot | Device status, installed-profile listing and switching, WiFi Calling controls, and SMS sending. Sensitive actions require administrator confirmation. |
| Operations | Authentication, CSRF protection, access policies, audit events, live logs, log retention, health checks, responsive layout, dark mode, and English/Chinese application UI. |
| Distribution | Static Linux binaries, systemd installation script, self-update with SHA-256 verification, Docker image, GHCR publishing, and GitHub Actions release builds. |

## Supported hardware

Vocat targets Qualcomm-based Quectel and China Mobile ML307 series modules that expose compatible AT, QMI, serial, and USB networking interfaces, including:

- Quectel EC20
- Quectel EC25
- Quectel EG25 family
- Compatible EG600 and related modules
- China Mobile ML307 series (e.g. ML307A-E5, various OEMs)

Available features depend on the module firmware, USB composition, SIM/eSIM capabilities, host drivers, radio network, and carrier configuration.

## Installation

### One-click Linux installation

As root (including OpenWrt/Kwrt, where `sudo` is normally absent):

```bash
curl -fsSL https://raw.githubusercontent.com/MengMengCode/VoCat/master/scripts/install.sh | bash
```

From a normal user on a distribution with sudo:

```bash
curl -fsSL https://raw.githubusercontent.com/MengMengCode/VoCat/master/scripts/install.sh | sudo bash
```

Check the host's VoWiFi/XFRM prerequisites without installing VoCat:

```bash
curl -fsSL https://raw.githubusercontent.com/MengMengCode/VoCat/master/scripts/install.sh | bash -s -- --check-env
```

Install a specific version:

```bash
curl -fsSL https://raw.githubusercontent.com/MengMengCode/VoCat/master/scripts/install.sh -o install.sh
sudo bash install.sh 0.0.2
```

VoWiFi IMS requires Linux XFRM/IPsec. On OpenWrt/Kwrt the installer attempts
to install matching `ip-full`, `kmod-ipsec`, `kmod-ipsec4/6`,
`kmod-crypto-authenc`, AES-CBC and SHA1 packages from the firmware's own feed.
If matching kernel modules are unavailable, use a firmware that includes them;
never force-install kmods built for a different kernel.

If your kernel cannot provide XFRM/IPsec and you only need non-VoWiFi features
such as cellular SMS or data, install with `--skip-vowifi-check`:

```bash
curl -fsSL https://raw.githubusercontent.com/MengMengCode/VoCat/master/scripts/install.sh -o install.sh
sudo bash install.sh --skip-vowifi-check
```

The installer:

- detects `amd64`, `386`, `arm64`, `aarch64`, or `armv7`;
- downloads the matching GitHub Release binary;
- verifies it against `SHA256SUMS`;
- installs Vocat under `/opt/vocat`;
- creates a hardened systemd service with the hardware and network access required by Vocat;
- stores runtime configuration in `/etc/vocat/env`;
- generates a random initial administrator password on first installation.

On systemd, installation requires two consecutive HTTP health responses from
the same service process before deleting the previous binary backup. Failed
upgrades restore that binary and check its health, retaining the backup for
diagnosis. The probe reads `VOCAT_ADDR` from `/etc/vocat/env`; deployments with
a different listener configuration can set `VOCAT_INSTALL_HEALTH_URL` when
running the installer. The private `/run/vocat` directory is preserved across
service stops so shared locks and unresolved ownership records remain available.

After installation, open:

```text
http://<server-address>:7575
```

### Manual binary installation

Download the matching binary and `SHA256SUMS` from GitHub Releases:

| Platform | Release file |
| --- | --- |
| Linux x86-64 | `vocat-linux-amd64` |
| Linux x86 32-bit | `vocat-linux-386` |
| Linux ARM64 | `vocat-linux-arm64` |
| Linux AArch64 | `vocat-linux-aarch64` |
| Linux ARMv7 | `vocat-linux-armv7` |

Verify and install it:

```bash
sha256sum -c SHA256SUMS --ignore-missing
sudo install -d -m 0755 /opt/vocat/bin /opt/vocat/data
sudo install -m 0755 vocat-linux-amd64 /opt/vocat/bin/vocat
read -rsp "Admin password: " VOCAT_BOOTSTRAP_PASSWORD; echo
printf '%s\n' "$VOCAT_BOOTSTRAP_PASSWORD" | sudo /opt/vocat/bin/vocat bootstrap-admin
unset VOCAT_BOOTSTRAP_PASSWORD
sudo env \
  VOCAT_DATABASE_PATH=/opt/vocat/data/vocat.db \
  /opt/vocat/bin/vocat serve
```

This manual command runs Vocat in the foreground. Use `vocat serve` so the
process starts the server directly; running `vocat` without arguments as root
on a TTY opens the interactive management menu instead. Use the one-click
installer when a managed systemd service and automatic restart are required.

### Docker

For a Linux host that must discover every attached supported Quectel modem and
continue seeing USB hot-plug events, run Vocat in hardware-access mode:

```bash
docker pull ghcr.io/mengmengcode/vocat:latest

read -rsp "Admin password: " VOCAT_BOOTSTRAP_PASSWORD; echo
printf '%s\n' "$VOCAT_BOOTSTRAP_PASSWORD" | docker run --rm -i \
  --user 0:0 \
  -v vocat-data:/opt/vocat/data \
  --entrypoint /opt/vocat/bin/vocat \
  ghcr.io/mengmengcode/vocat:latest bootstrap-admin
unset VOCAT_BOOTSTRAP_PASSWORD

sudo install -d -m 0700 /run/vocat
docker run -d \
  --name vocat \
  --restart unless-stopped \
  --network host \
  --pid host \
  --privileged \
  --user 0:0 \
  -v vocat-data:/opt/vocat/data \
  -v /dev:/dev \
  -v /sys:/sys:ro \
  -v /run/dbus:/run/dbus:ro \
  -v /run/vocat:/run/vocat \
  -v /run/lock:/run/lock \
  ghcr.io/mengmengcode/vocat:latest
```

Open `http://<server-address>:7575` after the container starts. Host networking
is required so QMI network interfaces remain visible to Vocat, while privileged
device access is required for serial ports, QMI control nodes, TUN interfaces,
network configuration, and devices added after the container starts. The
`/dev` bind mount makes new `ttyUSB*`, `ttyACM*`, `cdc-wdm*`, and MHI
`wwan*` nodes visible without recreating the container.

This mode intentionally gives Vocat broad access to the host's devices and
network stack. Use it only on a trusted Linux host. The automatic discovery
identifies supported Quectel USB modems (USB vendor ID `2c7c`) and China Mobile ML307 series modules by product string, and PCIe/MHI
modems exposed through the Linux WWAN subsystem; it does not identify arbitrary
modem layouts. Mapping only individual nodes with `--device`, such as
`/dev/ttyUSB2`, `/dev/cdc-wdm0`, or `/dev/wwan0qmi0`, limits the container to
those fixed nodes and does not provide complete multi-device or hot-plug discovery.

The GHCR image is published for `linux/amd64` and `linux/arm64`.

> [!TIP]
> **NAS / QNAP Container Station Deployment Note**:
> On NAS operating systems like QNAP QTS / QuTS hero (Container Station), custom non-root administrator accounts and volume isolation mechanisms may cause Docker named volumes (e.g. `-v vocat-data:/opt/vocat/data`) to resolve to different isolated paths between the one-off `bootstrap-admin` initialization and the daemon service container, leading to "Incorrect password" errors during Web login.
> For NAS environments, it is strongly recommended to replace named volumes with a host absolute path bind mount (e.g. `-v /share/Container/vocat/data:/opt/vocat/data` on QNAP) for both initialization and runtime to guarantee consistent SQLite database persistence.

### USB SIM readers

USB SIM readers use the Linux PC/SC service. The one-click installer installs
and starts `pcscd` plus the CCID driver automatically on supported package
managers. On Debian/Ubuntu, the equivalent manual setup is
`apt install pcscd libccid`. If USB sees a CCID reader but PC/SC is unavailable,
VoCat keeps the reader visible in the add-device dialog and reports the missing
service or driver instead of silently hiding it.

### QMI command-line utilities

VoCat uses `qmicli` to verify that a QMI control channel is ready and
`qmi-proxy` to multiplex access to it. Packet-data sessions are managed by the
built-in QMI WDS client instead of `qmi-network` CID/PDH state files. The
one-click installer installs and verifies the corresponding utilities. For manual deployment,
Debian/Ubuntu uses `apt install libqmi-utils`; Arch Linux uses
`pacman -S libqmi`, Alpine uses `apk add qmi-utils`, and OpenWrt uses
`opkg install qmi-utils`.

`vocat doctor --repair-dji-qmi` checks for `qmicli` before changing any USB
driver binding or asserting DTR. If the utility is unavailable, the command
stops with an installation hint and leaves the current device state untouched.

## Configuration

Vocat reads an optional JSON configuration file from `VOCAT_CONFIG`, then applies `VOCAT_*` environment variables. Environment variables take precedence.

| Environment variable | Default | Description |
| --- | --- | --- |
| `VOCAT_ADDR` | `0.0.0.0:7575` | HTTP listen address. |
| `VOCAT_DATABASE_PATH` | `./data/vocat.db` | SQLite database path. |
| `VOCAT_SESSION_TTL` | `24h` | Authentication session lifetime. |
| `VOCAT_SECURE_COOKIES` | `false` | Marks session cookies as secure when HTTPS is used. |
| `VOCAT_SHUTDOWN_TIMEOUT` | `10s` | Graceful shutdown timeout. |
| `VOCAT_MAX_REQUEST_BODY_BYTES` | `1048576` | Maximum API request body size. |
| `VOCAT_REPO` | `MengMengCode/VoCat` | Trusted GitHub repository used by the self-updater, in `owner/name` form. |
| `GITHUB_TOKEN` | empty | Optional GitHub token for private repositories or higher API limits. |

User-supplied Apple carrier bundles can be converted into reviewable,
allow-listed carrier profiles with `vocat carrier import-ipcc`; see
[docs/CARRIER_IPCC_IMPORT.md](docs/CARRIER_IPCC_IMPORT.md).

Administrator credentials are stored only in SQLite. Initialize an empty
database once with `vocat bootstrap-admin`; environment variables and JSON
configuration cannot set or overwrite the administrator username or password.

Do not store Telegram tokens, SMTP passwords, webhook secrets, SIM credentials, or other private data in the repository. Configure them through the application settings or protected environment files.

## Telegram bot

When Telegram notifications are enabled and both Chat ID and Admin ID are configured, the bot supports:

```text
/status [device]
/esim <device>
/switch <device> <iccid>
/wfc <device> <status|on|off|reconnect>
/sms <device> <number> <message>
/ussd <device> <code>
/ussd_reply <session> <input>
/ussd_cancel <session>
```

Profile switching and SMS submission use one-time confirmation buttons. The bot does not expose eSIM download, delete, or rename commands.

The web terminal and Telegram share USSD routing. With WiFi Calling enabled, requests use IMS/USSI and return a readiness error until IMS registers, without falling back to cellular AT+CUSD. With WiFi Calling disabled, requests use cellular USSD. Menu replies and cancellation stay bound to the original device and dialog. Cancellation errors are reported and retain the session for retry; an uncertain submission is never automatically resubmitted over another channel.

USSI dialog handling follows [3GPP TS 24.390](https://www.etsi.org/deliver/etsi_ts/124300_124399/124390/18.00.00_60/ts_124390v180000p.pdf). A failed BYE retains a cancellation record, but further menu input is blocked once cancellation has started ([RFC 3261, section 15.1.1](https://www.rfc-editor.org/rfc/rfc3261.html#section-15.1.1)).

## Updating

Check for a newer GitHub Release:

```bash
vocat update --check --repo MengMengCode/VoCat
```

Install the latest release:

```bash
sudo vocat update --repo MengMengCode/VoCat
```

The updater downloads the binary matching the current Linux architecture, verifies it with the published `SHA256SUMS`, replaces the executable atomically, and restarts the `vocat` systemd service when available.

For Docker installations:

```bash
docker pull ghcr.io/mengmengcode/vocat:latest
```

Recreate the container after pulling the new image.

## Development

Requirements:

- Go 1.25 or newer
- Node.js 20 or newer
- npm

Run the frontend development server:

```bash
cd web
npm install
npm run dev
```

Build the embedded frontend and start the backend:

```bash
cd web
npm run build
cd ..
go run ./cmd/vocat
```

Run all tests:

```bash
go test ./...
```

Build a production binary:

```bash
go build -trimpath -ldflags "-s -w" -o vocat ./cmd/vocat
```

## Release automation

Pushing a version tag starts two GitHub Actions workflows:

- `release-binaries` builds and publishes `amd64`, `386`, `arm64`, `aarch64`, and `armv7` binaries plus `SHA256SUMS`.
- `docker` builds and publishes a multi-architecture image to GitHub Container Registry.

```bash
git tag v0.2.0
git push origin v0.2.0
```

## Project layout

```text
cmd/vocat/                  Application entry point and CLI
internal/device/            Modem discovery and device control
internal/modem/             AT session and response handling
internal/server/            HTTP API, notifications, and embedded web server
internal/store/             SQLite persistence
internal/update/            GitHub Release self-updater
internal/vowifi/            IKE, EAP-AKA, IMS, and WiFi Calling runtime
scripts/install.sh          Linux installer and updater
web/src/                    React and TypeScript frontend
.github/workflows/          Binary and Docker release automation
```

## Responsible use

Cellular modem and eSIM operations can affect subscriber service, stored profiles, network registration, and hardware state. Keep backups, review destructive actions carefully, and use the software only in lawful environments where you are permitted to operate the connected hardware and network resources.

Vocat does not bypass carrier authentication, network policy, hardware security, or eSIM trust requirements. Support for an operation means that Vocat can request it from the modem or eUICC; the device, profile, network, or carrier may still reject it.

## Contributing

Issues and pull requests are welcome. Keep changes focused, include tests where practical, avoid committing credentials or subscriber data, and document hardware-specific behavior clearly.

Before submitting a change:

```bash
go test ./...
cd web && npm run build
```

## Thanks
- [Nodeseek.com](https://www.nodeseek.com) — A community dedicated to servers
- [Linux.do](https://linux.do) — An inspiring tech community
- [iniwex5](https://github.com/iniwex5) — Style and Functionality Guidelines

## Buy me a coffee

| Network | Address |
| ------- | ------- |
| USDT-TRON (TRC20) | `TWSAkvzVsFc7KqncDLmUfRxpPQbpV5CgTB` |
| USDT-BSC (BEP20) | `0xb43031387342ebb1ff536fb9ad6440b9e6377139` |
| USDT-Polygon | `0xb43031387342ebb1ff536fb9ad6440b9e6377139` |

## License

See [LICENSE](LICENSE).

## Star History

<a href="https://www.star-history.com/?repos=mengmengcode%2Fvocat&type=date&legend=top-left">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/chart?repos=mengmengcode/vocat&type=date&theme=dark&legend=top-left" />
   <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/chart?repos=mengmengcode/vocat&type=date&legend=top-left" />
   <img alt="Star History Chart" src="https://api.star-history.com/chart?repos=mengmengcode/vocat&type=date&legend=top-left" />
 </picture>
</a>
