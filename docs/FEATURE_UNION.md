# VoWin／VoCat 能力并集与 Linux 补齐清单

本次要求：VoCat-sip 作为 Linux 端，成熟 VoWin 为主要行为基准；通话与音频、多模组与
恢复、管理界面与通知三个方向都要补齐。保留 VoCat 已有功能，少量 Windows 缺口另列。
此前 SimAdmin Rust 选型记录保留为历史，不再用它代替本次 Go 工作。

以下为当前工作区源码核对，不把旧 README、接口存在或历史绿色记录视为本次验收。
“已接入”仅表示业务入口已连接；运营商／硬件／跨网段验收单独记录。

## 能力矩阵

| 能力 | VoWin 基准或 VoCat 自有能力 | VoCat 当前状态与差距 |
| --- | --- | --- |
| 独立模组状态与控制 | Windows `VoKernelService.TelephonyModes.cs`、`ModuleManagementInstance` | `ims_dispatch.go`、`ims_mode_api.go` 已按模组分派；实际热拔、换卡、冷启动恢复仍须补验收 |
| 蜂窝模式、VoWiFi 与默认选路独立 | Windows `SimTelephonyPreferences` 分开保存蜂窝／WiFi 自动连接及默认路由 | 已写入逐卡策略、双运行时分派、原通话／USSI 会话归属、Web 控制、逐卡启动投递、换卡停止确认、Host IMS 实时 SIM 元数据、模块 RF／NAS 原卡核验及原数据会话清理；任务完整恢复和部分管理生命周期仍在整合。最新应用改动尚未统一测试／构建，不能视为完成。见[独立接入边界](INDEPENDENT_ACCESS.md)与[数据交接](CELLULAR_DATA_HANDOVER.md) |
| Host IMS 信令与短信 | VoSharp Host IMS 会话、承载与安全所有者 | Go `hostimsruntime` 已接应用；真实多机固件、完整冷启动与运营商业务仍待解决／验收 |
| VoWiFi 注册与恢复 | VoSharp IKE／Manager／按卡历史 | Go `vowifi` 已有双栈、按卡候选、认证证据、持久退避与自动恢复；DITO VoWiFi 实测注销 SIP 500、跨 reader／多卡槽及实机故障矩阵仍待解决／验收 |
| 网页／本机电话 | Windows 电话页、浮窗与麦克风会话 | Go `web_phone.go`＋`web/src/phone` 已有拨接挂、独占 PCM、跨页来电卡、静音和电平；浏览器休眠、真实听感仍需验收 |
| USB 模组双向媒体 | Windows `CellularAudioBridge`、`UsbAudioModemBinding`、`Qdc507VoiceRuntime` | Go `modulecall` 已接显式 module 模式的网页、SIP 入出呼和 Telegram 原通话控制；先取得同物理设备双向 ALSA 再 ATD／ATA，精确 Call-ID 与串口结束版本隔离。软件回环与实机业务验收分开，见 [USB 语音边界](MODULE_VOICE.md) |
| 通话中按键 | Windows 电话键盘／控制入口 | 网页 IMS 按键和 SIP 分机／peer 的 RFC 4733 跨腿已接线；新增 SIP INFO `application/dtmf-relay` 到原运营商通话的兼容入口、对话核验与防重。INFO 新源码待最后统一验证；运营商 IVR 及真实软电话仍待验收 |
| 保持与恢复 | Windows `ImsCallManager.HoldAsync/ResumeAsync` 通过 re-INVITE | 已写入精确 IMS 对话的出站 re-INVITE、ACK／迟到回复处理、媒体方向与网页保持／恢复入口；新增源码尚未统一测试／构建，见[保持边界](IMS_CALL_HOLD.md) |
| 自动接听与留言播放 | Windows `CallMessagePlayback`、`AutoAnswerMessageRequests`、来电延时设置 | 新源码已接按 ICCID 保存、5–120 秒延时、文件导入／试听、统一应答仲裁、ACK／模块确认、PCM 播放、原通话挂断和网页接管；尚未统一测试／构建，见[自动接听边界](AUTO_ANSWER.md) |
| 通话录音 | Windows `CallExperienceSettings.SaveCallRecordings`、录音目录、最近 20 通保留与通话媒体保存 | 新源码已接 Wi-Fi／Host IMS 与原 USB 通话 PCM 旁路录音、双声道 WAV、启停／保留／容量设置、试听／下载／删除及中断恢复；尚未统一测试／构建，见[录音边界](CALL_RECORDINGS.md) |
| SIP 内线与来显 | VoSharp SIP gateway；Go 自有 B2BUA | Go 已接 Registrar、Digest、内线群振首接和双向 G.711；软电话实机与高级对话功能仍待验收 |
| SIP 运营商入呼／外呼 | 两端已有逐线权限、选线和呼叫归属 | Go 已接 IMS、显式 USB module＋PCM 和 RFC 4733 跨腿；群振首接、双向 G.711／IMS 事件回环已有测试，SIP INFO、保持及 RTCP 仍待补齐 |
| 可选 Asterisk peer | 双向独立凭据、分方向通话／短信权限 | Go 固定 peer 与本地 Registrar 共存；隔离互通已有历史证据，生产／跨网段媒体验收另列 |
| 原卡短信与回复绑定 | Windows `SipMessages`、SQLite 准入与来源收据 | Go 已有 MO 防重、MT 队列、canonical 快照和原 SIM 回复，不能以应用提交代替运营商送达 |
| 分段与验证码 API | Windows `SmsApiService`／`SmsVerificationCodeExtractor` | Go `smsapi`／`smsquery` 已实现；短信 MESSAGE 可见分段不同于 UDH 透明重组；真实调用端和误判反馈待验收 |
| 多模组轮询隔离 | Windows 独立模组实例与恢复 | 本次将 CLCC 轮询改为最多四个独立 worker；通知发送前释放该模组模式读锁。仍不等于全部 reader／网络／SIM 生命周期已对齐 |
| 来电通知独立性 | Windows 接收事件和独立提醒 | 本次修复 Go 全进程按号码去重：IMS 按设备＋接入方式＋Call-ID；每个 Server 独立，有界记录；同号码下一通不合并 |
| 通知渠道故障隔离 | Go 自有 Telegram、邮件、Webhook、Bark、PushPlus、企业微信、飞书、MeoW | 本次来电渠道并发，各有 10 秒总限时；短信原已有逐渠道游标。通知可靠投递／平台真实响应仍另验收 |
| 状态与按需诊断 | Windows 灯态、逐卡 IKE 原因与诊断包 | Go 已有逐模组模式／IKE 状态、按需详情；本次修复 Host IMS 来电误标 VoWiFi，键盘能力取真实 SDP，不取“已注册”猜测 |
| eSIM、USSD、AT 工具 | 两端已有实现，Go 包含多种设备驱动 | 保留 Go `esim_api.go`／`ussd.go`／`device` 工具；各固件／PCSC／QMI 兼容与并发独占需继续核对 |
| 网络、代理与流量管理 | Go 自有供网、出口代理、流量和 Web 管理 | 保留这些 Go 能力；Windows 是否需要等效供网／代理控制须逐项对齐，不能仅由菜单推断完成 |
| 自动任务和多通知渠道 | Go 自有保活任务、时区、任务记录与重试 | 保留 Linux 功能；新增同模组任务独占、原呼叫结束确认和准备／回切失败的按卡恢复，尚未统一验证；持久恢复与 Host IMS 任务交接仍待收尾。Windows 多渠道／任务管理等效能力另列反向核对，见[任务边界](AUTOMATIC_TASKS.md) |
| 发布与回滚 | Windows 轻量包；Go 多架构嵌入 Web、Docker／systemd | 最终新源码／新前端一起构建；安装、升级、受控回滚及双模组实网仍独立于软件测试 |

## 实施顺序与通过条件

**2026-10-26 当前开发提交：** 按要求先实现、最后统一测试；本次 README 与 Git 整理
未执行测试或构建。独立接入新增源码处于整合阶段，早先运行时／媒体阶段的绿色结果不能
代替此次应用层验收。本清单保留所有尚未补齐和实机待验收项。

1. **当前批次：通话按键、来电通知和轮询隔离。** 需要实际 RTP 回环、调用链／通话归属、
   并发／取消、真实组件渲染与前后端构建。结果见本次验收记录；不是整体并集完成。
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

本批软件验证与保留失败见[验收记录](../../publish/vocat-feature-union-20261026/README.md)。
USB 语音接线的后续软件验证见[独立记录](../../publish/vocat-module-voice-20261026/README.md)。
SIP 按键跨腿的后续验证见[独立记录](../../publish/vocat-sip-dtmf-20261026/README.md)。
原许可证与授权检查保持。
