<p align="center">
  <img src="../web/public/favicon.svg" width="96" alt="Vocat">
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

[English](../README.md) | **简体中文** | [العربية](README.ar.md) | [繁體中文](README.zh-TW.md) | [Français](README.fr.md) | [Русский](README.ru.md) | [Español](README.es.md) | [日本語](README.ja.md)

Vocat 是一款面向 Quectel EC20/EC25 系列蜂窝模组的开源 Web 控制面板与工程工具套件。它在一个自包含的服务中整合了模组发现、实时射频状态、AT 与 USSD 终端、短信、WiFi Calling（WiFi 通话）、eSIM 管理、网络选择、代理路由、通知、审计日志以及发布自动化。

后端使用 Go 编写，界面采用 React 与 TypeScript 构建，生产环境前端被嵌入进 Go 二进制中。单个可执行文件即包含完整的 Web 应用，并使用 SQLite 进行持久化存储。

## 当前开发状态（2026-10-27）

最新纯 Host IMS 接通音频、最终响应缺少重复 SDP 的修复、显式 Host IMS 自动任务及
恢复验收见[通话与任务补充记录](VERIFICATION_20261027_HOSTIMS_CALL_TASKS.md)。
后续纯 Host IMS 保持／恢复、来电自动接听、提示音播放、原通话 PCM 接管和双向录音已完成
分项实测，见[高级通话验收](VERIFICATION_20261027_HOSTIMS_ADVANCED.md)。按键已确认提交，
远端 IVR 识别与实际浏览器麦克风／扬声器仍单独验收。
新增[切卡任务恢复验收](VERIFICATION_20261027_ESIM_TASKS.md)：修正 eUICC 管理 AID 误用和
重启后缺少身份快照导致的恢复阻塞；已实测已安装配置的注册／回切，以及切卡后强杀主进程
自动恢复原配置和偏好，被中断任务保持失败且不重放业务。
后续[定时切卡与 LPA 边界验收](VERIFICATION_20261027_ESIM_BOUNDARIES.md)已通过真实时钟触发、
完整请求写入后与卡片成功响应后两种强杀恢复；均重新核实原配置与偏好，未重放中断业务。
新增[空数据库标准安装验收](VERIFICATION_20261027_FRESH_INSTALLER.md)：在显式隔离硬件下，
首次生成管理员、初始密码仅打印一次，以及重装保留原密码／会话均通过；宿主机 AMR-NB 依赖已补装并验证。
新增 [SIP 与纯 Host IMS 联合验收](VERIFICATION_20261027_HOSTIMS_SIP.md)：真实 Baresip 分机经
纯 Host IMS 接通，接听者确认提示音和说话，手机 5／8 按键到达分机；SIP 发起短信已送达，
人工 OK 回复入库并获授权 SIP 接收方确认。首次回拨无人接听的记录保留；随后
[回拨复测](VERIFICATION_20261027_HOSTIMS_SIP_INCOMING.md)已通过 PCMA 分机接听、双向语音、
按键 8 与另一分机取消振铃。素材播放结束触发分机 BYE，原通话均已清理；手机主动挂断路径另验收。

本 fork 作为 Linux 客户端，参考成熟的 [VoWin](https://github.com/unikgyd/VoWin) 补齐能力，
同时保留现有 Linux 功能。通话与音频、多模组与恢复、管理与通知三个方向均在范围内。

QDC507 按 VoWin 当前方案使用纯 **Host IMS**，由主机负责 IMS 会话、短信与 RTP／AMR
媒体。旧 QDC507 ADB／D4／UAC 语音桥接已废弃，不属于本次 Linux 补齐目标。

| 方向 | 当前源码状态 |
| --- | --- |
| 通话与音频 | 已接入网页电话、同物理设备 USB PCM、SIP 分机／peer、RFC 4733 按键。 |
| 多模组与恢复 | 已有独立运行时所有权、Host IMS、按卡 ePDG 历史与持久重试准入。 |
| 独立接入 | 已实现按 ICCID 保存蜂窝／Wi-Fi 意愿、默认选路、原会话归属、分别重连和界面；统一软件测试与构建已通过。 |
| 管理 | 保留 eSIM、USSD、网络／代理、任务、短信 API 和通知；已接逐卡启动、Host IMS 实时身份、原数据会话／传输清理、任务持久恢复、Host IMS 交接及共存模式的数据／代理控制。 |
| 高级通话 | 已接 IMS 保持／恢复、legacy SIP INFO、按卡延时接听、留言导入／播放、网页接管、录音及独立 RTCP／媒体统计；已验证的软件范围见验收记录。 |
| Linux 实机 | QDC507／电信 Host IMS 与 EC20／DITO VoWiFi 已同时注册；纯 Host IMS 短信收到状态码 0 的运营商送达报告；后续 AMR 通话已接通并有双向 RTP，接听者确认短音清楚且说了话。 |
| 后续实测 | 已通过真实 Baresip 分机双向媒体／转码、RTCP 和按键、局域网 MESSAGE 确认／防重、物理 USB PCM 读写、已安装 eSIM 配置往返切换及四种留言格式导入的分项检查。 |
| 蜂窝数据出口 | 电信 CTNET 的 IPv6、IPv4V6 和 IPv4 已通过 HTTP／SOCKS5 认证代理的真实 HTTPS、IPv4／IPv6 公网地址查询及原数据资源清理。 |
| 原生安装 | 限定场景下的真实 systemd 安装、失败升级回滚、原管理员保留和共享锁保留已通过。 |
| 高级实机通话 | 纯 Host IMS 保持／恢复、来电自动接听、提示音播放、网页电话 PCM 接管和完整双声道录音已通过限定场景；协商按键已获提交确认。 |
| SIP 运营商实测 | PCMU 外呼与 PCMA 入呼经纯 Host IMS AMR 接通，双向语音、手机按键回传、首接取消及分机挂断清理已通过；短信 OK 回复闭环已通过。 |
| 尚待验收 | 实际浏览器音频、其他 SIP 挂断／故障场景、远端 IVR 识别、USB 通话媒体及其他故障／硬件／安装组合。 |

首轮按先实现、最后统一测试的顺序，通过了 **2025 项原生 Go、2020 项静态 Go、79 项前端、
132 次关键竞态、41 项隔离内核检查、vet 和四个 Linux 构建**。跳过项、保留的失败与
实机边界见[统一软件验收记录](VERIFICATION_20261026.md)。随后进行的 NAS 实机测试、
兼容性修复、通话结果和回滚见[Linux 实机验收记录](VERIFICATION_20261027_LIVE.md)。
后续场景、实测发现的问题与剩余项目见[部署服务补充验收](VERIFICATION_20261027_FOLLOWUP.md)。
最新候选的 2057 项 Go 检查、竞态／内核检查、双架构构建、真实数据出口、依赖要求和
发布权限检查见[IPv6 与双栈验收](VERIFICATION_20261027_IPV6.md)。
后续 2,062 个 Go 测试、真实 systemd 升级回滚及 IPv4／IPv6 公网查询修复见
[原生安装补充验收](VERIFICATION_20261027_NATIVE.md)。
原 EC20／DITO SIM 在隔离 Docker 容器中经过真实 SIGKILL 后，按保存的连接设置恢复了
隧道和 IMS 注册；经过、清理及宿主网络崩溃恢复的剩余边界见
[容器崩溃补充验收](VERIFICATION_20261027_CRASH.md)。
[QMI 归属对照](VERIFICATION_20261027_QMI_OWNERSHIP.md)补充了新客户端读取 IPv4／IPv6／
双栈状态的 14 次实机观察；该阶段尚未注入真实崩溃，后续恢复结果见下方记录。
新增[普通数据持久化保护](DATA_OWNERSHIP_FENCE.md)：按原卡启动 QMI 前先记录所有者，
未确认释放的记录阻止新进程接管；恢复仍须由可证明的原始所有者完成。
可选[独立数据所有者](DATA_OWNER.md)现已完成宿主网络命名空间内的主进程真实 SIGKILL、
IPv4／IPv6／双栈自动恢复与原网络清理，见[完整记录](VERIFICATION_20261027_DATA_OWNER.md)。
原电信卡的 Host IMS 也完成一次保持宿主网络与原 ModemManager 的主进程 SIGKILL，
约 45.8 秒自动恢复真实注册；原生 IMS 设置已恢复，见[实测记录](VERIFICATION_20261027_HOSTIMS_PROCESS.md)。
数据所有者自身崩溃、MM／主机重启和断电恢复仍另列验收。
新增[原生数据所有者与 DNS 验收](VERIFICATION_20261027_NATIVE_OWNER_DNS.md)：真实 systemd
IPv4／双栈 IPv4 恢复及蜂窝 DNS 修复已通过；随后 IPv6 崩溃测试发现自动启动的 QMI
代理会随主服务退出。已补独立代理服务与启动就绪检查。
后续[独立原生部署检查](VERIFICATION_20261027_NATIVE_INDEPENDENT.md)已通过真实受限代理单元、
客户端崩溃隔离及空库管理员／服务初始化；正常停启后原密码与会话保留，临时安装已清理。
这些隔离检查不替代运营商恢复及标准安装脚本的空机验收。
随后[原生三服务实测](VERIFICATION_20261027_NATIVE_COLD.md)已通过 IPv4、IPv6 和双栈
主进程 SIGKILL 自动恢复，原失败现场也已受控恢复。初始 IPv6 不回包仍需单独维护重启，
这一初始化问题尚未解决，不能计为自动修复。
新增[通话空闲判断与运营商复测](VERIFICATION_20261027_CALL_IDLE.md)：已修正非语音 CLCC
记录误阻拦线路／任务的问题，真实手动与定时注册、三种数据恢复及 VoWiFi 连接均通过。
电信卡一条短信获模块接受，一次模块拨号振铃后清理；短信送达和接通媒体仍未确认。
菲律宾卡未拨号、未发短信。

新增[纯 Host IMS 复测](VERIFICATION_20261027_PURE_HOSTIMS.md)：已更正 QDC507 目标并补上
原卡 UIM 的短信中心读取。冻结候选通过 2,086 项 Go 用例、120 次相关并发检查、内核检查、
vet 和四种构建；一条纯 Host IMS 短信获 IMS 接受，并收到运营商送达报告。此前 module／
ADB 音频和模块短信均不计入 Host IMS 验收，接通音频仍待实测。

完整差异见[能力并集清单](FEATURE_UNION.md)，接入边界见[独立接入说明](INDEPENDENT_ACCESS.md)，
源码、缓存和运行文件的提交规则见[仓库约定](REPOSITORY.md)。原许可、地区准入和授权检查保持。
本次受控电信测试卡通过[精确 SIM 准入文件](AUTHORIZED_TEST_SIMS.md)配置，真实身份不提交到 Git。
现有 IMS Profile、模组 IMS 交接和依赖要求见[Host IMS 部署说明](HOST_IMS_DEPLOYMENT.md)。
本轮统一验收逐项记录在[验收台账](PARITY_ACCEPTANCE.md)，Host IMS 已确认日志的清理边界见[重启恢复](HOST_IMS_RECOVERY.md)。

自动接听的格式、时限、首接仲裁与网页接管见[实现边界](AUTO_ANSWER.md)。
通话录音的 PCM 采集、保留、私有目录与中断恢复见[录音说明](CALL_RECORDINGS.md)。

<p align="center">
  <img src="../img/image.png">
  <img src="../img/image-1.png">
</p>

## 功能

| 领域 | Vocat 提供的能力 |
| --- | --- |
| 设备管理 | 自动串口/USB 发现、多模组支持、设备友好名称、概览实时刷新、模组重启、飞行模式以及 USB 网卡模式控制。 |
| 射频与网络 | 注册状态、运营商、信号指标、RSRP/RSRQ/SINR、网络模式、频段、信道、运营商扫描以及自动/手动选网。 |
| AT 与 USSD | 交互式 AT 终端、命令历史、原始模组响应、USSD 发起/继续/取消流程以及清晰的模组错误上报。 |
| 短信 | 蜂窝与 IMS 短信直接发送、入站同步、分段短信处理、送达报告、会话历史、未读状态、时间戳以及逐条消息的送达状态。 |
| WiFi Calling | IKEv2/ePDG 隧道建立、EAP-AKA 鉴权、IMS 注册、IMS 短信、重连控制、状态诊断以及按设备路由。 |
| eSIM 与 eUICC | eUICC 发现、EID 与生产信息、证书元数据、多 eUICC 清单、已安装配置文件列表、启用/禁用/切换操作，以及在卡片支持时进行下载、重命名和删除。 |
| 卡策略 | 基于 ICCID 的 WiFi Calling 与飞行模式行为，策略即时应用。 |
| 代理路由 | 上游 SOCKS 路由、设备绑定、国家规则、TCP 可达性检查以及面向 WiFi Calling 数据路径的 UDP Associate 检查。 |
| 通知 | 通过 Telegram、Bark、邮件、Pushplus 以及签名 Webhook 转发新入站短信，每条短信单独推送。 |
| Telegram 机器人 | 设备状态、已安装配置文件列表与切换、WiFi Calling 控制以及短信发送。敏感操作需要管理员确认。 |
| 运维 | 鉴权、CSRF 防护、访问策略、审计事件、实时日志、日志留存、健康检查、响应式布局、深色模式以及中英文应用界面。 |
| 分发 | 静态 Linux 二进制、systemd 安装脚本、带 SHA-256 校验的自更新、Docker 镜像、GHCR 发布以及 GitHub Actions 发布构建。 |

## 支持的硬件

Vocat 面向基于高通芯片、并暴露兼容 AT、QMI、串口与 USB 网络接口的 Quectel 模组，包括：

- Quectel EC20
- Quectel EC25
- Quectel EG25 系列
- 兼容的 EG600 及相关模组

可用功能取决于模组固件、USB 复合设备配置、SIM/eSIM 能力、主机驱动、无线网络以及运营商配置。

## 安装

### Linux 一键安装

已是 root（包括默认没有 `sudo` 的 OpenWrt/Kwrt）：

```bash
curl -fsSL https://raw.githubusercontent.com/MengMengCode/VoCat/master/scripts/install.sh | bash
```

普通 Linux 用户且系统装有 sudo：

```bash
curl -fsSL https://raw.githubusercontent.com/MengMengCode/VoCat/master/scripts/install.sh | sudo bash
```

只检查 VoWiFi/XFRM 环境，不安装 VoCat：

```bash
curl -fsSL https://raw.githubusercontent.com/MengMengCode/VoCat/master/scripts/install.sh | bash -s -- --check-env
```

安装指定版本：

```bash
curl -fsSL https://raw.githubusercontent.com/MengMengCode/VoCat/master/scripts/install.sh -o install.sh
sudo bash install.sh 0.0.2
```

VoWiFi IMS 必须使用 Linux XFRM/IPsec。OpenWrt/Kwrt 上安装脚本会从当前固件自己的软件源尝试安装严格匹配的 `ip-full`、`kmod-ipsec`、`kmod-ipsec4/6`、`kmod-crypto-authenc`、AES-CBC 和 SHA1 组件。若软件源没有与当前内核匹配的模块，必须更换包含这些组件的固件，禁止强装其他内核版本的 kmod。

如果内核无法提供 XFRM/IPsec，而你只需要蜂窝短信或数据等非 VoWiFi 功能，可使用 `--skip-vowifi-check` 安装：

```bash
curl -fsSL https://raw.githubusercontent.com/MengMengCode/VoCat/master/scripts/install.sh -o install.sh
sudo bash install.sh --skip-vowifi-check
```

安装程序会：

- 检测 `amd64`、`386`、`arm64`、`aarch64` 或 `armv7` 架构；
- 下载对应的 GitHub Release 二进制；
- 对照 `SHA256SUMS` 进行校验；
- 将 Vocat 安装到 `/opt/vocat`；
- 创建具有 Vocat 所需硬件与网络访问权限的强化版 systemd 服务；
- 将运行时配置存放在 `/etc/vocat/env`；
- 首次安装时生成随机初始管理员密码。

systemd 安装会等待同一个服务进程连续两次通过 HTTP 健康检查，之后才删除旧程序
备份。升级启动失败会恢复旧二进制并再次检查健康状态，同时保留备份供诊断。
检查地址从 `/etc/vocat/env` 的 `VOCAT_ADDR` 读取；采用其他监听配置时，可在运行
安装脚本时指定 `VOCAT_INSTALL_HEALTH_URL`。服务停止后会保留私有的 `/run/vocat`
目录，使共享锁及尚未确认清理的资源记录仍然可用。

安装完成后打开：

```text
http://<server-address>:7575
```

### 手动二进制安装

从 GitHub Releases 下载对应的二进制与 `SHA256SUMS`：

| 平台 | 发布文件 |
| --- | --- |
| Linux x86-64 | `vocat-linux-amd64` |
| Linux x86 32 位 | `vocat-linux-386` |
| Linux ARM64 | `vocat-linux-arm64` |
| Linux AArch64 | `vocat-linux-aarch64` |
| Linux ARMv7 | `vocat-linux-armv7` |

校验并安装：

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

该手动命令会在前台运行 Vocat。请使用 `vocat serve` 以直接启动服务器；在 TTY 下以 root 运行无参数的 `vocat` 会进入交互式管理菜单。如需托管的 systemd 服务与自动重启，请使用一键安装脚本。

### Docker

如果 Linux 主机需要发现每一个接入的受支持 Quectel 模组，并持续感知 USB 热插拔事件，请以硬件访问模式运行 Vocat：

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

容器启动后打开 `http://<server-address>:7575`。必须使用主机网络，才能让 QMI 网络接口保持对 Vocat 可见；串口、QMI 控制节点、TUN 接口、网络配置以及容器启动后新增的设备则需要特权设备访问。`/dev` 绑定挂载使新的 `ttyUSB*`、`ttyACM*`、`cdc-wdm*` 和 MHI `wwan*` 节点无需重建容器即可见。

该模式有意赋予 Vocat 对主机设备和网络栈的广泛访问权限，请仅在受信任的 Linux 主机上使用。自动发现会识别受支持的 Quectel USB 模组（USB 厂商 ID `2c7c`），以及通过 Linux WWAN 子系统暴露的 PCIe/MHI 模组；它不能识别任意模组布局。仅用 `--device` 映射单个节点，例如 `/dev/ttyUSB2`、`/dev/cdc-wdm0` 或 `/dev/wwan0qmi0`，会将容器限定在这些固定节点上，无法提供完整的多设备或热插拔发现。

GHCR 镜像发布为 `linux/amd64` 与 `linux/arm64`。

> [!TIP]
> **NAS / QNAP Container Station 部署说明**：
> 在 QNAP QTS / QuTS hero（Container Station）等 NAS 操作系统上，自定义的非 root 管理员账号和卷隔离机制可能导致 Docker 命名卷（例如 `-v vocat-data:/opt/vocat/data`）在一次性的 `bootstrap-admin` 初始化和后台服务容器中解析为不同的隔离路径，从而在 Web 登录时出现“密码错误”。
> 在 NAS 环境中，强烈建议初始化和运行时均以主机绝对路径的绑定挂载替代命名卷（例如 QNAP 上的 `-v /share/Container/vocat/data:/opt/vocat/data`），以保证 SQLite 数据库持久化存储的一致性。

### USB SIM 读卡器

USB SIM 读卡器使用 Linux PC/SC 服务。一键安装程序会在支持的软件包管理器上自动安装并启动 `pcscd` 及 CCID 驱动。Debian/Ubuntu 上等效的手动安装命令为 `apt install pcscd libccid`。如果 USB 已识别 CCID 读卡器，但 PC/SC 不可用，VoCat 会继续在添加设备对话框中显示该读卡器，并报告缺少的服务或驱动，而不是静默隐藏它。

### QMI 命令行工具

VoCat 使用 `qmicli` 验证 QMI 控制通道是否就绪，并使用 `qmi-proxy` 复用对该通道的访问。分组数据会话由内置 QMI WDS 客户端管理，而非使用 `qmi-network` 的 CID/PDH 状态文件。一键安装程序会安装并验证相应工具。手动部署时，Debian/Ubuntu 使用 `apt install libqmi-utils`；Arch Linux 使用 `pacman -S libqmi`，Alpine 使用 `apk add qmi-utils`，OpenWrt 使用 `opkg install qmi-utils`。

`vocat doctor --repair-dji-qmi` 会在更改任何 USB 驱动绑定或置位 DTR 之前检查 `qmicli`。如果该工具不可用，命令会停止并给出安装提示，保持设备当前状态不变。

## 配置

Vocat 先从 `VOCAT_CONFIG` 读取可选的 JSON 配置文件，再应用 `VOCAT_*` 环境变量。环境变量优先级更高。

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `VOCAT_ADDR` | `0.0.0.0:7575` | HTTP 监听地址。 |
| `VOCAT_DATABASE_PATH` | `./data/vocat.db` | SQLite 数据库路径。 |
| `VOCAT_SESSION_TTL` | `24h` | 鉴权会话有效期。 |
| `VOCAT_SECURE_COOKIES` | `false` | 在使用 HTTPS 时将会话 Cookie 标记为安全。 |
| `VOCAT_SHUTDOWN_TIMEOUT` | `10s` | 优雅关闭超时时间。 |
| `VOCAT_MAX_REQUEST_BODY_BYTES` | `1048576` | API 请求体最大字节数。 |
| `VOCAT_REPO` | `MengMengCode/VoCat` | 自更新器使用的受信任 GitHub 仓库，格式为 `owner/name`。 |
| `GITHUB_TOKEN` | 空 | 可选的 GitHub token，用于私有仓库或更高的 API 限额。 |

用户提供的 Apple 运营商配置包可通过 `vocat carrier import-ipcc` 转换为可审查、符合允许列表的运营商配置文件；参见 [docs/CARRIER_IPCC_IMPORT.md](CARRIER_IPCC_IMPORT.md)。

管理员凭据仅存储在 SQLite 中。使用 `vocat bootstrap-admin` 对空数据库执行一次初始化；环境变量和 JSON 配置不能设置或覆盖管理员用户名或密码。

请勿将 Telegram token、SMTP 密码、Webhook 密钥、SIM 凭据或其他私密数据存放在仓库中。请通过应用设置或受保护的环境文件来配置它们。

## Telegram 机器人

启用 Telegram 通知并配置好 Chat ID 与 Admin ID 后，机器人支持：

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

配置文件切换与短信提交使用一次性确认按钮。机器人不暴露 eSIM 下载、删除或重命名命令。

网页终端与 Telegram 共用 USSD 路由。启用 WiFi Calling 时，请求使用 IMS/USSI；IMS 未就绪时会明确报错并提示等待注册，不会回退到蜂窝 AT+CUSD。关闭 WiFi Calling 时使用蜂窝 USSD。菜单回复与取消始终绑定原设备和会话。取消失败会返回错误并保留会话供重试；发送结果不确定时不会自动换通道重新提交。

USSI 会话处理遵循 [3GPP TS 24.390](https://www.etsi.org/deliver/etsi_ts/124300_124399/124390/18.00.00_60/ts_124390v180000p.pdf)。BYE 失败时保留取消记录，但开始取消后禁止继续提交菜单输入（[RFC 3261 第 15.1.1 节](https://www.rfc-editor.org/rfc/rfc3261.html#section-15.1.1)）。

## 更新

检查是否有更新的 GitHub Release：

```bash
vocat update --check --repo MengMengCode/VoCat
```

安装最新发布版：

```bash
sudo vocat update --repo MengMengCode/VoCat
```

更新器会下载与当前 Linux 架构匹配的二进制，使用已发布的 `SHA256SUMS` 进行校验，原子性地替换可执行文件，并在可用时重启 `vocat` systemd 服务。

Docker 安装的更新方式：

```bash
docker pull ghcr.io/mengmengcode/vocat:latest
```

拉取新镜像后重建容器。

## 开发

依赖要求：

- Go 1.25 或更新版本
- Node.js 20 或更新版本
- npm

运行前端开发服务器：

```bash
cd web
npm install
npm run dev
```

构建嵌入的前端并启动后端：

```bash
cd web
npm run build
cd ..
go run ./cmd/vocat
```

运行全部测试：

```bash
go test ./...
```

构建生产二进制：

```bash
go build -trimpath -ldflags "-s -w" -o vocat ./cmd/vocat
```

## 发布自动化

推送版本标签会触发两个 GitHub Actions 工作流：

- `release-binaries` 构建并发布 `amd64`、`386`、`arm64`、`aarch64` 与 `armv7` 二进制及 `SHA256SUMS`。
- `docker` 构建并向 GitHub Container Registry 发布多架构镜像。

```bash
git tag v0.2.0
git push origin v0.2.0
```

## 项目结构

```text
cmd/vocat/                  应用入口与 CLI
internal/device/            模组发现与设备控制
internal/modem/             AT 会话与响应处理
internal/server/            HTTP API、通知与内嵌 Web 服务器
internal/store/             SQLite 持久化
internal/update/            GitHub Release 自更新器
internal/vowifi/            IKE、EAP-AKA、IMS 与 WiFi Calling 运行时
scripts/install.sh          Linux 安装与更新脚本
web/src/                    React 与 TypeScript 前端
.github/workflows/          二进制与 Docker 发布自动化
```

## 负责任地使用

蜂窝模组与 eSIM 操作可能影响用户服务、已存储的配置文件、网络注册以及硬件状态。请做好备份，谨慎审视破坏性操作，并仅在您被允许操作所连接的硬件与网络资源的合法环境中使用本软件。

Vocat 不会绕过运营商鉴权、网络策略、硬件安全或 eSIM 信任要求。支持某项操作意味着 Vocat 能够向模组或 eUICC 发起该请求；但设备、配置文件、网络或运营商仍可能拒绝。

## 贡献

欢迎提交 Issue 与 Pull Request。请保持改动聚焦，在可行处附带测试，避免提交凭据或用户数据，并清晰地说明硬件相关行为。

提交改动前：

```bash
go test ./...
cd web && npm run build
```

## 致谢
- [Nodeseek.com](https://www.nodeseek.com) — 专注服务器的社群
- [Linux.do](https://linux.do) — 富有启发的技术社群
- [iniwex5](https://github.com/iniwex5) — 风格与功能指南

## 请我喝杯咖啡

| 网络 | 地址 |
| ------- | ------- |
| USDT-TRON (TRC20) | `TWSAkvzVsFc7KqncDLmUfRxpPQbpV5CgTB` |
| USDT-BSC (BEP20) | `0xb43031387342ebb1ff536fb9ad6440b9e6377139` |
| USDT-Polygon | `0xb43031387342ebb1ff536fb9ad6440b9e6377139` |

## 许可证

参见 [LICENSE](../LICENSE)。

<a href="https://star-history.dera.page/#MengMengCode/VoCat">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://star-history.dera.page/svg?repos=MengMengCode/VoCat&theme=dark" />
   <source media="(prefers-color-scheme: light)" srcset="https://star-history.dera.page/svg?repos=MengMengCode/VoCat" />
   <img alt="Star History Chart" src="https://star-history.dera.page/svg?repos=MengMengCode/VoCat" />
 </picture>
</a>
