# Linux USB 模组语音

显式选择 `ims_mode=module` 后，支持的模组可通过本模组 USB Audio 的双向 PCM 接入网页
电话和 SIP 分机／peer。Telegram 控制同一通话，不采集 Telegram 端音频。VoWiFi 和
Host IMS 仍使用原来的会话控制器。没有配对 UAC 的旧 AT 调用接口保留为信令工具，
网页和 SIP 不会降级到只有 ATD／ATA、没有媒体的呼叫。
没有 UAC 的模组仍保留 Telegram 原有的信令工具；已有 USB 通话一旦取得所有权，即使
之后就绪检查失败，Telegram 也只能通过原通话 ID 清理，不能切回通用 ATA／ATH。

## 准入和归属

状态页只读设备快照和 sysfs，不打开 PCM、不查询 SIM。实际呼叫取得物理租约后重新读
ICCID／IMSI、检查地区授权和 CLCC，固定 USB 物理路径、枚举代次、AT 会话及 SIM。
必须先独占打开同模组的 capture/playback，并确认 8 kHz、单声道、S16_LE，才提交
ATD／ATA。CLCC 确认 active 后才启动 PCM；AT 的 OK 只表示接受命令。

ALSA 软件参数显式设置 capture 的启动阈值为 1 帧、playback 为 160 帧，避免系统默认
800 帧阈值使 20 ms 非阻塞采集永远停留在 PREPARED。配置阶段不开始读写，首个实际
PCM I/O 才启动流。该问题由物理 UAC 诊断暴露；依据为 [ALSA PCM 启动阈值文档](https://alsa.alsa-project.org/alsa-doc/alsa-lib/pcm.html)。

每通电话使用 `usb-` 随机 Call-ID；网页、SIP 群振和 Telegram 共用它。每个物理模组
同一时刻只接管一通非多方语音电话。重启后不接管已经 active 的外部电话。接听前必须
还是同一通 ringing 来电，只有首接方取得媒体。网页 DTMF 使用已附着音频的浏览器，
通过原租约提交 AT+VTS；IMS 线路保持协商式 RFC 4733。

挂断先关闭媒体，再重新核对原 CLCC 索引／方向／号码，使用 `AT+CHLD=1X` 释放该索引；
没有宽泛 ATH 兜底。串口从已缓冲数据观察到 NO CARRIER／BUSY／NO ANSWER 或重启指示，
会改变通话结束版本；写命令前再次检查版本，防止旧 ATA／挂断影响后一通电话。索引释放
和拨号 OK 的语义依据 [Quectel EC25／EC21 AT 手册](https://quectel.com/content/uploads/2021/03/Quectel_EC25EC21_AT_Commands_Manual_V1.3.pdf)。

释放命令一旦可能写到串口，只轮询确认结果，不自动重发 CHLD；用户再次请求挂断也不会
重复未知操作。原索引若被后来电话复用，旧清理任务不能靠重试对它再次发命令。

换卡、物理替换、配置重新绑定会撤销旧媒体。无法确认结束时保留清理状态和物理租约，
新拨号、模式切换、飞行模式、重启、eSIM 操作和通用原始 AT 写入会被拒绝。只允许只读
CLCC 观察；数据库查询超时本身不表示换卡，不会因此释放租约。应用关闭等待已经准入的
呼叫准备，再对原 ID 清理；超时不能算作已确认挂断。
AT 等待同时受原通话寿命约束；关闭超时后会取消仍在等待设备锁的动作，不能借请求方
尚未过期的上下文在应用停止后继续拨号。

设备层同时阻止敏感 AT 通道、按串口改 USB 模式、蜂窝 IMS 重启、SIM reset、运营商
扫描／重注册、APN 注册准备、锁小区写入和 USSD 绕过通话所有者。状态读取不需占用媒体。

## 运行与验收

需要 Linux 的 `snd-usb-audio`、可访问的 `/sys` 与 `/dev/snd`、系统 `libasound.so.2`，
以及模组固件已经将通话音频路由到 USB Audio。程序不猜测 QDC507 ADB／专有固件指令，
不会选择 PCH、桌面麦克风或默认声卡。`vocat_static` 的 USB 原生 ALSA 后端不可用，
能力检查会明确拒绝，不以无声电话代替。

`usb_audio.voice_bridge_ready` 是兼容的只读检查字段，仍为 false；端点配对不证明电话
已经接通。网页显示“模组语音可连接”，实际电话的 `media_ready` 才表示当前租约已启动。

软件验证覆盖真实 HTTP／WebSocket、SIP UDP／RTP 双向 G.711↔PCM、群振首接、原源控制、
结束版本、取消、并发、关闭时序，以及 Linux ALSA NULL 插件 ABI（不是物理声卡）。
这不证明运营商音频听感、实际 UAC 固件路由、热拔换卡、同模组多通话或保持恢复。
真实拨入拨出、双向听感与逐设备生命周期仍需单独验收。

本批证据和剩余限制见[验证记录](../../publish/vocat-module-voice-20261026/README.md)。
