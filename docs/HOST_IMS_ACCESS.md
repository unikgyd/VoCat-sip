# Host IMS：PreparedAccess 与单线路 runtime 接线

2026-10-24。本阶段衔接[同承载安全边界](HOST_IMS_OWNED_SECURITY.md)，将已有的
SIM、NAS/WDS、内核网络、安全 owner 与 IMS Provider 组合成可调用的内部 API。
这不是主应用已开放 Host IMS 模式的声明，也不代表运营商注册、实卡通话或短信验收完成。
原有 LICENSE、地区、设备及 SIM 策略继续保留。

## 已接上的内部路径

`hostims.PrepareAccess(ctx, lease)` 只接受调用者已经通过 `BearerPool.Acquire` 获得的
同一条 IMS PDN 租约。它不自动选模组、不取得另一条承载，不启用 RF、不改变模组内置
IMS 策略，也不发送 REGISTER。构造 SIM/NAS source 等早期校验失败可以返回 `nil, err`；
这时调用者仍拥有原 lease，必须继续保留并负责其清理，不能把 `nil` 当成承载已释放。

准备阶段的次序是：

1. 在该 lease 上读取并固定卡身份及 ISIM/USIM 配置。
2. 通过自有 NAS/WDS CID 获取同一查询的 LTE cell 和实际 PDN/P-CSCF 证据。
3. 准备同源 host 地址、路由和 socket 绑定，再准备同承载的安全 scope。
4. 仅在上述条件成立后发布 `PreparedAccess` 的接入证据；IMS 注册由外层 runtime 发起。

同一个 `PreparedAccess` 同时实现 `CellularIMSBearer`、`CellularIMSSecurity` 和
`PreferredAKAProvider`。网络、安全和 AKA 不允许来自相似地址、另一个 owner 或另一张卡。
卡身份 getter 返回 detached 数据；固定卡的 ICCID/IMSI、设备身份、归属 MCC/MNC 与
原 lease 一致，UIM 事务自身仍核对同卡、slot、应用及物理来源。预先检查一次卡身份，
不能代替 AUTHENTICATE 事务内的前后核验；不自动重放结果未知的 AKA。

`NASCellQueryProof.RuntimeProof()` 提取的是**同一次 NAS 查询所包含的 WDS 证据**，不是
另一次独立读取拼出的结果。返回结构不共享可变 runtime 数据，后续 Runtime Verify 重绑
同 lease 的普通 WDS source，不捕获原 NAS 查询的 context/session，也不会暗中刷新 cell
的查询时间。准备出的内核网络仍保留来源 proof 和 fingerprint。

## 准备完成不等于 IMS 已注册

| 层次 | 成功表示 | 不表示 |
| --- | --- | --- |
| `BearerLease` | 自有 IMS PDN 已建立并且归属可核验 | SIM AKA、host 路由或 SIP 已就绪 |
| `PreparedAccess.Evidence().Established` | 有效查询样本、同承载网络和安全准备成立 | 运营商已接受 REGISTER |
| `hostimsruntime` 的 `registering` | 正在启动真实 IMS Provider | 已有可交付的注册会话 |
| `hostimsruntime` 的 `ready` | 返回的 IMS Session 报告 `Registered=true` | 已通过实卡通话、短信或多模组验收 |

`internal/hostimsruntime` 提供 `NewRuntime(Config)`、`Start`、`Stop`、`Snapshot` 和
`Session`，管理**一条**线路。`Config.Prepare` 返回该线路的 Access；runtime 用它同时作为
AKA provider 和 `IMSRequest.Cellular`，不构造虚假的 SWu tunnel。IMS 配置强制
`SecurityRequired`，不使用无关的全局 IPSec installer；DOMAIN/IMPI/IMPU 来自固定卡，
不能由配置中的猜测身份覆盖。

只有 Provider 启动成功且 Session 的注册证据成立，runtime 才进入 `ready` 并允许借用
`Session()`。借用不转移生命周期所有权；业务调用者应停止 runtime，不应自行关闭其 Session。
`Snapshot` 不暴露卡身份或未处理的底层错误；已失去注册的会话不会继续显示 ready。

## 有界查询年龄：当前产品策略

当前 Access 消费查询样本采用以下本地边界：

- NAS Cell 请求/响应窗口不超过 **5 秒**。
- 消费样本时，从主机 **query START** 到当前时刻不超过 **10 秒**；不能改成从读取 getter、
  发布快照或最后一次 Verify 成功开始计时。未来时间、反向时间或过期样本不通过。
- `PreparedAccess.VerificationTimeout()` 请求 **20 秒**的有界核验预算。IMS 对显式预算接口
  允许的上界是 25 秒；未提供合法预算的 bearer 仍使用原 3 秒。调用者更早的 deadline
  始终有效；20 秒是核验/操作预算，不是延长样本有效期，也不是整条线路的注册成功时限。

这些数值是**本项目产品策略，不是 3GPP 或 QMI 标准规定**。`QueryWindow` 记录主机发出
请求和接收响应的时间，不是模组报告的测量时间，也不能得出无线测量恰好只有十秒年龄。
样本在内存中使用主机时间关系核验；重启后不能从账本恢复其“新鲜”状态。

QMI `Get Cell Location Info` 的公开 LTE TLV 提供 PLMN、TAC、ECI、EARFCN 和 PCI，但没有
测量时间戳或穷尽所有小区变化的连续序号。因此将其作为本次有界 solicited 查询消费，
不要求无法证明的全程连续稳定，也不声称查询后、SIP 发送期间或远端收到时小区绝对未变。
`ContinuousCellCoverage()` 仍为 false；不会用 MM 缓存、IMSI 归属 PLMN 或静态 UI 配置
替代服务小区。[libqmi 官方 NAS schema](https://github.com/linux-mobile-broadband/libqmi/blob/main/data/qmi-service-nas.json)

一次 `PreparedAccess.Verify` 获取新的 NAS 样本及其配套 WDS 证据，再与初始 PDN
fingerprint、固定卡和内核 owner 快照配对核验；不会在该组合步骤里额外叠加完整的
WDS Verify。普通 socket/media 消费者原有的来源核验不因此全局关闭。

## 正常换小区与 PANI

同一条 PDN、固定卡、地址及 P-CSCF 来源没有变化时，新的合法 NAS 样本可以包含不同
服务小区；不要求后续 cell 永远等于首次注册时的 cell。新的成功核验证据会更新 Go IMS
会话的 UE-provided PANI，供后续 REGISTER、INVITE、MESSAGE 及相关对话请求使用；
失败样本不能覆盖为一个看似有效的新位置。

PANI 描述 UE 正在使用的接入网络，不是从卡归属推算的永久位置；这与
[RFC 7315 §4.4](https://www.rfc-editor.org/rfc/rfc7315.html#section-4.4) 的用途一致。
本实现不把 UE 提供的小区值标成 `network-provided`，也不将其连续稳定性作为已证明事实。

Windows 现有 C# 路径已经有 `SipRegisterSession.RefreshAccessNetworkInfoAsync`，在注册及
呼叫准备时刷新可选 cellular PANI；相关预算到期时省略旧值，而不是继续使用陈旧小区。
本轮未修改 C#；Linux 的强制同承载证据要求，不等于重新定义 Windows 可选元数据路径。

## 错误、取消与关闭责任

准备 API 可能返回 **非 nil owner 和 error**。调用者必须同时处理两者，保留 owner 并调用
或重试 `Close`；不能只按 error 分支丢弃对象。worker 迟到返回、未知 CID/内核 ACK 或
清理未确认时，原 bearer claim 仍保留资源和归属，不能靠 GC、超时或“内核里看起来相同”
的对象认领/释放它们。早期返回 nil 的情况，原 lease 的责任仍在外层调用者。

`hostimsruntime` 会保留 Prepare 或 Provider 返回的非 nil Access/Session，包括同时返回错误
的情况。启动取消不抛弃迟到对象；关闭使用独立有界收尾。`Stop` 先关闭 IMS Session，
确认后才关闭 Access；Session 关闭失败时，两者都保留。Access 再依次处理保留媒体引用、
安全 scope、host 网络，最后释放自己拥有的 PDN。

`Close`/`Stop` 失败不是“已停止但可以重启”。`cleanup_failed` 后保留同一 owner/reference，
再次 `Stop` 重试相同资源；不能创建第二条线路覆盖旧引用、绕过 pending fence，或在仍有
IMS I/O/未知 ACK 时先拆下层网络。公开裸 `BearerLease.Close` 也不能绕过活动 PreparedAccess。

停止路径**不进入 VoWiFi RFOff，不关闭或恢复整个无线电，不执行模组内置 IMS 恢复、重启、
切卡或基带重配置**。它只注销/关闭本程序的 IMS 会话并释放本程序拥有的资源；不借用
现有 VoWiFi Orchestrator，因为后者的启动策略会关闭蜂窝 RF 和数据连接。

## 还没有完成的应用与实机接入

上述是实际可调用的内部组合层，不是已经连到主应用的模式开关。以下项目仍需完成：

- 主应用 DeviceID 到 ModemManager 物理目标的可靠映射、共享 BearerPool 和 lease 管理。
- 每模组的 Host IMS/VoWiFi 模式选择，以及普通数据、SIM/eSIM 维护之间的互斥。
- 服务器呼叫/短信/媒体 dispatch、SIP 网关对新 runtime 的分派及 UI 状态接入。
- 对真实固件的 NAS 布局、EARFCN、查询语义和实际耗时验收，以及实卡注册、通话、短信、
  多模组联合验收；不将合成 responder 的成功当成这些验收已通过。
- 完整 cold boot 后的 PDN、网络、应用生命周期与恢复接线。现有 ACK-owned cleanup-only
  恢复能力不是自动重建原会话；pending/unknown ACK 仍不得推断成功或 adopt。

本阶段最终源码冻结及验证材料目录是工作区 `publish/vocat-prepared-access-20261024/`。
最终测试结果以 `verification.json` 为准，顶层用例不重复加子项，重复次数单列：

- Go 全仓 1334 通过／2 跳过；Windows 四包 race 十轮 3270 通过。
- Linux 原生 race：Host IMS 178 通过／36 guard 跳过、Access／NAS 十轮 320 通过、
  runtime 十轮 160 通过、IMS 205 通过／6 内核 guard 跳过。
- 新 NET＋MNT 的网络／清理／TC 三轮 105、scoped XFRM 9、双 owner 3、socket ESP
  12、数据路径三轮 144，全部通过且无跳过。最终验收无失败。
- Go Linux amd64／arm64 构建及静态检查通过；本阶段不把交叉构建当作 CLI／运营商运行。
  C# 源码未改，只验证前阶段源码基线与 201 份共享镜像一致。

真实 Provider 的 loopback 用例验证 AKA digest、安全能力调用、注册、注销和资源关闭次序，
但其 ESP hook 是合成的。它与隔离内核机制测试是两类证据，不能拼称实卡端到端成功。
