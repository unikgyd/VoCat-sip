# VoWin／VoCat 能力并集与 Linux 补齐清单

本次要求：VoCat-sip 作为 Linux 端，成熟 VoWin 为主要行为基准；通话与音频、多模组与
恢复、管理界面与通知三个方向都要补齐。保留 VoCat 已有功能，少量 Windows 缺口另列。
此前 SimAdmin Rust 选型记录保留为历史，不再用它代替本次 Go 工作。

以下为当前工作区源码核对，不把旧 README、接口存在或历史绿色记录视为本次验收。
“已接入”仅表示业务入口已连接；运营商／硬件／跨网段验收单独记录。
本轮统一软件结果见[验收记录](VERIFICATION_20261026.md)和[验收台账](PARITY_ACCEPTANCE.md)。
真实硬件、运营商和部署项目单独保留，不由软件通过结果代替。

QDC507 的当前 VoWin 基准是纯 Host IMS：主机持有蜂窝 IMS 承载、认证、SIP 会话与
RTP／AMR 媒体。用户已明确废弃旧 `Qdc507VoiceRuntime` ADB／D4／UAC 桥接方案；
它不属于本次 Linux 补齐目标。QDC507 的通话、短信与音频必须通过显式 `host_ims`
线路验收，不能以 ATD／USB 音频、ADB 短音或模块短信结果代替。

2026-10-27 的[数据与退出实测](VERIFICATION_20261027_NETWORK.md)补入电信 CTNET
真实 WDS 数据、电脑经 HTTP CONNECT／SOCKS5 的认证 HTTPS 流量、代理关闭时的原隧道
取消、数据关闭后的路由／DNS／公网 IP 缓存清理，以及新版服务重启自动恢复和活动隧道下
正常退出。定时与手动任务均实际触发，但因模组存在非语音活动记录被正确拒绝，尚不是任务
业务成功证据。该轮恢复宿主环境时 ModemManager 崩溃并自动重启一次，故障保留。

随后[IPv6／双栈实测](VERIFICATION_20261027_IPV6.md)补齐独立 WDS 双地址族、IPv6
地址／路由／DNS 和原状态恢复，并修复串行地址尝试耗尽连接时间的问题。最终同一候选已通过
IPv6、IPv4V6 和 IPv4 的真实 HTTPS／代理与清理检查。匹配的临时 ModemManager 修复版
在数据测试及释放端口归属期间保持同一进程、零自动重启，随后恢复原宿主服务。
这不替代突然崩溃恢复、物理热拔插及运营商接通／短信业务验收。

随后补入[普通数据持久化保护](DATA_OWNERSHIP_FENCE.md)，并完成统一软件检查和同候选的
CTNET IPv4／IPv6／双栈连接、日志释放、公网 IP 查询与原环境恢复，见
[验收记录](VERIFICATION_20261027_DATA_FENCE.md)。这是原始所有者的持久屏障；
丢失 WDS 连接后的自动清理仍待补齐，不能记作活动数据崩溃恢复通过。

可选[独立数据所有者](DATA_OWNER.md)进一步保留原始 QMI／宿主配置及主进程实例锁副本，
主进程被强杀后先确认清理，再让保存的 IPv4／IPv6／双栈意图自动恢复。
[实机记录](VERIFICATION_20261027_DATA_OWNER.md)包含三个完整宿主网络场景、首次 SQLite
锁问题的修复和启动重试观察；这不覆盖所有者自身崩溃或断电恢复。

## 能力矩阵

| 能力 | VoWin 基准或 VoCat 自有能力 | VoCat 当前状态与差距 |
| --- | --- | --- |
| 独立模组状态与控制 | Windows `VoKernelService.TelephonyModes.cs`、`ModuleManagementInstance` | `ims_dispatch.go`、`ims_mode_api.go` 已按模组分派；实际热拔、换卡、冷启动恢复仍须补验收 |
| 蜂窝模式、VoWiFi 与默认选路独立 | Windows `SimTelephonyPreferences` 分开保存蜂窝／WiFi 自动连接及默认路由 | 已写入逐卡策略、双运行时分派、原通话／USSI 会话归属、Web 控制、逐卡启动投递、换卡停止确认、Host IMS 实时 SIM 元数据、模块 RF／NAS 原卡核验、任务持久恢复及原数据会话／传输清理。应用改动已通过本轮软件检查，实机生命周期仍待验收。见[独立接入边界](INDEPENDENT_ACCESS.md)与[数据交接](CELLULAR_DATA_HANDOVER.md) |
| Host IMS 信令与短信 | VoSharp Host IMS 会话、承载与安全所有者 | Go `hostimsruntime` 已接应用；已补原卡 UIM 短信中心读取，纯 Host IMS 短信实测获得状态码 0 的运营商送达报告，见[复测](VERIFICATION_20261027_PURE_HOSTIMS.md)。已确认旧日志按网络／安全／原 PDN 顺序清理，未知日志保留屏障；主进程真实 SIGKILL 与自动注册恢复已有[实测](VERIFICATION_20261027_HOSTIMS_PROCESS.md)。后续纯 Host IMS AMR 通话已接通，接听者确认短音清楚，主机收到上行 PCM 和双向 RTP，见[通话补充](VERIFICATION_20261027_HOSTIMS_CALL_TASKS.md)；MM／主机重启及短信人工回复仍另验收 |
| VoWiFi 注册与恢复 | VoSharp IKE／Manager／按卡历史 | Go `vowifi` 已有双栈、按卡候选、认证证据、持久退避与自动恢复；DITO VoWiFi 实测注销 SIP 500、跨 reader／多卡槽及实机故障矩阵仍待解决／验收 |
| 网页／本机电话 | Windows 电话页、浮窗与麦克风会话 | Go `web_phone.go`＋`web/src/phone` 已有拨接挂、独占 PCM、跨页来电卡、静音和电平；纯 Host IMS 出呼、回拨自动接听及原通话 PCM 接管已分项实测，见[高级通话验收](VERIFICATION_20261027_HOSTIMS_ADVANCED.md)；浏览器实际设备链路与休眠仍需验收 |
| USB 模组双向媒体 | 显式 module 模式的兼容能力；不作为 QDC507 的 Host IMS 基准 | Go `modulecall` 已接显式 module 模式的网页、SIP 入出呼和 Telegram 原通话控制；先取得同物理设备双向 ALSA 再 ATD／ATA，精确 Call-ID 与串口结束版本隔离。保留兼容能力，但不再移植已废弃的 QDC507 ADB 桥接；QDC507 单独走 Host IMS，见 [USB 语音边界](MODULE_VOICE.md) |
| 通话中按键 | Windows 电话键盘／控制入口 | 网页 IMS 按键和 SIP 分机／peer 的 RFC 4733 跨腿已接线；新增 SIP INFO `application/dtmf-relay` 到原运营商通话的兼容入口、对话核验与防重。纯 Host IMS 原通话按键已确认 HTTP 200／submitted，见[实测](VERIFICATION_20261027_HOSTIMS_ADVANCED.md)；运营商 IVR 识别及真实软电话仍待验收 |
| 保持与恢复 | Windows `ImsCallManager.HoldAsync/ResumeAsync` 通过 re-INVITE | 已写入精确 IMS 对话的出站 re-INVITE、ACK／迟到回复处理、媒体方向与网页保持／恢复入口；纯 Host IMS 同一通话已实测 held／恢复确认及恢复后的短音听感，见[高级通话验收](VERIFICATION_20261027_HOSTIMS_ADVANCED.md)与[保持边界](IMS_CALL_HOLD.md) |
| 自动接听与留言播放 | Windows `CallMessagePlayback`、`AutoAnswerMessageRequests`、来电延时设置 | 已接按 ICCID 保存、5–120 秒延时、文件导入／试听、统一应答仲裁、ACK／模块确认、PCM 播放、原通话挂断和网页接管；纯 Host IMS 回拨已实测延时接听、ACK 确认、提示音播放和原通话 PCM 接管，见[实测](VERIFICATION_20261027_HOSTIMS_ADVANCED.md)；实际浏览器按钮／设备链路仍待验收，见[实现边界](AUTO_ANSWER.md) |
| 通话录音 | Windows `CallExperienceSettings.SaveCallRecordings`、录音目录、最近 20 通保留与通话媒体保存 | 已接 Wi-Fi／Host IMS 与原 USB 通话 PCM 旁路录音、双声道 WAV、启停／保留／容量设置、试听／下载／删除及中断恢复；纯 Host IMS 出呼和来电已生成完整录音，提示音／接收 PCM 抽样匹配，实际运营商 WAV 的隔离 API 鉴权、范围读取、设置和副本删除已通过，见[实测](VERIFICATION_20261027_HOSTIMS_ADVANCED.md)与[录音边界](CALL_RECORDINGS.md) |
| RTCP 与媒体质量观察 | VoSharp `RtpSession` 的独立控制端口、接收报告与来源约束 | 新源码已接 IMS／Host IMS 和 SIP 网关的独立 RTCP、SDP 控制端口、SR／RR／SDES／BYE、丢包／抖动／RTT、本通话来源核验及原承载清理；软件套件已通过，实机互通另验收，见[RTCP 边界](RTCP.md) |
| SIP 内线与来显 | VoSharp SIP gateway；Go 自有 B2BUA | Go 已接 Registrar、Digest、内线群振首接和双向 G.711；软电话实机与高级对话功能仍待验收 |
| SIP 运营商入呼／外呼 | 两端已有逐线权限、选线和呼叫归属 | Go 已接 IMS、显式 USB module＋PCM 和 RFC 4733 跨腿；新源码已接 SIP INFO 与 RTCP，保持／恢复的 IMS 路径已实现；当前协议整合软件套件已通过，实机互通另验收，历史群振和媒体回环记录不代替本次验收 |
| 可选 Asterisk peer | 双向独立凭据、分方向通话／短信权限 | Go 固定 peer 与本地 Registrar 共存；隔离互通已有历史证据，生产／跨网段媒体验收另列 |
| 原卡短信与回复绑定 | Windows `SipMessages`、SQLite 准入与来源收据 | Go 已有 MO 防重、MT 队列、canonical 快照和原 SIM 回复，不能以应用提交代替运营商送达 |
| 分段与验证码 API | Windows `SmsApiService`／`SmsVerificationCodeExtractor` | Go `smsapi`／`smsquery` 已实现；短信 MESSAGE 可见分段不同于 UDH 透明重组；真实调用端和误判反馈待验收 |
| 多模组轮询隔离 | Windows 独立模组实例与恢复 | 本次将 CLCC 轮询改为最多四个独立 worker；通知发送前释放该模组模式读锁。仍不等于全部 reader／网络／SIM 生命周期已对齐 |
| 来电通知独立性 | Windows 接收事件和独立提醒 | 本次修复 Go 全进程按号码去重：IMS 按设备＋接入方式＋Call-ID；每个 Server 独立，有界记录；同号码下一通不合并 |
| 通知渠道故障隔离 | Go 自有 Telegram、邮件、Webhook、Bark、PushPlus、企业微信、飞书、MeoW | 本次来电渠道并发，各有 10 秒总限时；短信原已有逐渠道游标。通知可靠投递／平台真实响应仍另验收 |
| 状态与按需诊断 | Windows 灯态、逐卡 IKE 原因与诊断包 | Go 已有逐模组模式／IKE 状态、按需详情；本次修复 Host IMS 来电误标 VoWiFi，键盘能力取真实 SDP，不取“已注册”猜测 |
| eSIM、USSD、AT 工具 | 两端已有实现，Go 包含多种设备驱动 | 保留 Go `esim_api.go`／`ussd.go`／`device` 工具；各固件／PCSC／QMI 兼容与并发独占需继续核对 |
| 网络、代理与流量管理 | Go 自有供网、出口代理、流量和 Web 管理 | 新源码将 HTTP／SOCKS 与公网 IP 查询绑定到已确认的原卡数据会话，停止／任务／换卡前取消并等待传输；共存 Wi-Fi 保留数据开关，代理区分监听与可供网，软件套件已通过，实机互通另验收。Windows 是否需要等效供网／代理控制仍另列反向核对 |
| 自动任务和多通知渠道 | Go 自有保活任务、时区、任务记录与重试 | 保留 Linux 功能；新增同模组任务独占、原呼叫结束确认、数据库恢复日志、重启后只恢复环境、Host IMS 任务交接和排队补投；本轮补齐 API／界面的显式 Host IMS 短信／通话任务选择和 schema 33 迁移，已实测定时 IMS 短信送达、通话任务结束与原策略恢复、SIGKILL 后无重放恢复，验收见[通话与任务补充](VERIFICATION_20261027_HOSTIMS_CALL_TASKS.md)。后续已修正管理 AID 误用与冷启动身份快照阻塞，已安装配置的注册／回切及切卡后 SIGKILL 自动恢复原配置、偏好和日志已通过[实测](VERIFICATION_20261027_ESIM_TASKS.md)。Windows 多渠道／任务管理等效能力另列反向核对，见[任务边界](AUTOMATIC_TASKS.md) |
| 发布与回滚 | Windows 轻量包；Go 多架构嵌入 Web、Docker／systemd | 最终新源码／新前端一起构建；安装、升级、受控回滚及双模组实网仍独立于软件测试 |

## 实施顺序与通过条件

**2026-10-26 当前开发候选：** 已按要求先实现，再完成本轮统一软件测试与构建。
结果、跳过项和保留失败见上方记录；早先快照不替代此次结果。真实固件／SIM 生命周期、
软电话／运营商互通、USB 音频和部署仍须单独验收。

1. **通话按键、来电通知和轮询隔离。** 已纳入 RTP 回环、调用链／通话归属、
   并发／取消、组件渲染与前后端检查；实际平台投递和运营商验收另列。
2. **USB 语音业务接线（已实现，软件证据见独立记录，真实通话仍待验收）。** 在 ATD／ATA
   前取得所选物理设备的双向音频租约；网页、SIP 和 Telegram 共用原通话所有者。挂断、
   掉线、换卡、热拔关闭原媒体，禁止默认声卡兜底。没有 UAC 的旧 AT 工具仍是信令工具。
3. **独立接入策略和生命周期。** 迁移现有互斥模式，明确蜂窝／WiFi 自动连接和默认路由，
   保持逐模组故障隔离；真实硬件验证停止、注销、冷启动、重连和换卡。
4. **高级呼叫与提醒。** 保持／恢复、自动接听留言、通话录音、SIP INFO 兼容；RFC 4733 分机／peer
   跨腿已接入，见 [SIP 按键边界](SIP_DTMF.md)。继续统一首接与媒体所有权，操作未知不重发。
5. **剩余双向管理差异与联合验收。** eSIM／USSD reader 矩阵、Go 自有任务／通知的 Windows
   等效能力、软电话／运营商真实双向媒体短信、构建发布与回滚。

VoWin 运行代码在本批次保持原样。没有通过降低目标、改写跳过项或移除失败断言宣布全量
对齐；本清单中仍写“待实现”的能力不是完成项。

## 本次接口边界

`POST /api/phone/dtmf` 使用已有管理登录与 CSRF，要求 `device_id`、精确 `call_id` 和一个
`digit`。只有当前附着音频的同一浏览器可提交；既有 IMS dispatch 使用原通话控制器，
不会根据之后的默认线路把按键转给另一会话。断开音频／停止／通话结束取消在途按键。
IMS 没有对端 telephone-event/8000 协商就拒绝；显式 USB 模组模式使用原租约 AT+VTS，
不在操作失败后切到另一通道。没有 INFO 或带内音自动兜底。

当前为固定 160 ms 事件、20 ms 更新、三次结束报告、一次只允许一个按键且不排队。
RTP 接收的 `ReadDTMF` 已接 SIP 跨腿，接管前清掉旧键、320 ms 后的积压键不重放。协议依据为
[RFC 4733](https://datatracker.ietf.org/doc/html/rfc4733)，未将专有 SIP INFO DTMF 宣称为标准。

本轮软件验证与保留失败见[统一验收记录](VERIFICATION_20261026.md)。
较早阶段的原始日志保存在本机归档，不作为克隆仓库的链接依赖。
原许可证与授权检查保持。
