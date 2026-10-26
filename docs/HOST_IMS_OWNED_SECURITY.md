# Host IMS：同承载安全边界与重叠地址隔离

2026-10-24。后续于[网络消费者接线](HOST_IMS_DATAPATH.md)。本阶段补真实安全边界，
不是开放 Linux Host IMS 业务模式。所有原 LICENSE、地区／设备／SIM／评估检查保留。
没有实际模组查询、运营商 REGISTER、电话、短信或生产服务变更；不用 WSL。

## 已接入的消费者约束

蜂窝 IMS 的 `Start` 在任何网络操作前要求同一 bearer 实现 `CellularIMSSecurity`。
protected TCP／UDP listener 在 **bind/listen 前** 要求 owner 安装 required ESP socket policy；
保护连接的重连和 accepted TCP 也必须通过同一能力。初始未保护 REGISTER 和 RTP 只用
原有网络绑定，不错误套用 SIP 的 ESP socket policy。MSS hook 仍与 owner hook 组合。

SA 安装改由同一 owner 的 `InstallIPSec` 执行；蜂窝路径不能调用 Provider 的全局默认
`ip xfrm` installer，也不能在缺少能力／绑定失败时退回明文或默认网卡。VoWiFi 的既有
隧道／installer 路径保留，不将蜂窝能力要求施加到 Wi-Fi。

独立 `imssec` 包只有标准库依赖，提供旧类型 aliases、四条 transport ESP state、八条
TCP／UDP policy 的共同计划与精确校验。补上原来缺失的 UE-client **UDP 入站 policy**。
`BuildScopedPlan` 支持 owner 提前保留的两条唯一 reqid，才能与首次 REGISTER 前已装好的
socket 策略一致；不能仅从协商 SPI 的异或值推断多模组归属。计划不含密钥。

SA 安装后的取消／错误若伴有 handle，不会丢弃它。清理使用独立十秒预算，失败保留并能
重复 Close；启动失败、不能向调用者交付 Session 时，显式 `RetainIPSec` 把未完成清理
移交长期 bearer owner，并使该 owner 的 Verify／新 socket／安装保持隔离，直到独立确认
清理。不会继续尝试另一个 endpoint／transport，也不能由另一张卡代为回收。

Linux 已新增私有生产组件 `kernelSecurityLease`，同时实现同一 owner 的网络和安全能力。
`prepareKernelSecurity` 只接受已有来源证明的内核网络租约，不接受 UI 指定 mark、namespace、
接口或 P-CSCF。这不是 `CellularIMSBearer`／Established 证据工厂，也没有把完整 Host IMS
模组模式接入业务开关；消费者缺少完整能力时仍明确失败。

## Linux 原生机制与低层 helper

隔离实测确认：相同 IP／SPI 的两个 SA 不能仅靠 `if_id` 或 selector.Ifindex 区分；
错误 if_id 的 Get／Delete 也不是安全的归属检查。full-mask mark 可以分离同地址 SA，
但必须在入站 SA 查找前由已证明的 ingress 设置；只给 socket 设置 mark 不够。

`RequireSocketESP` 直接给已验证 raw socket 设置 `IP_XFRM_POLICY`／`IPV6_XFRM_POLICY`，
IN／OUT 都 required transport ESP，template 精确 local／remote 和 owner reqid。
特别注意：socket policy 的 selector 必须覆盖该 socket **全部流量**；只匹配精确对端的
selector 实测会让其他来源明文回落 global policy。helper 已覆盖此负测。

helper 自身核验 family／type／protocol／已 bind 的本地地址端口，允许 pre-bind Control；
失败必须关闭 socket，不回滚为明文。TCP listener 的 policy 由内核继承给 accepted。
它不创建 SA、TC 或全局 policy，也不证明调用者传入的 reqid 已拥有，不能单独作为 bearer。

原生合成 ESP 验证覆盖 UDP4／UDP6、不同来源明文、错误 reqid、临时对端端口、双向 ESP、
删除 OUT SA 后不泄漏明文及 TCP accepted 继承；全部只发生在新 NET＋MNT namespace。
详细机制实验、原始日志、可复现脚本与一手内核源码依据见工作区
`publish/vocat-xfrm-scope-audit-20261024/AUDIT.md`。

## 私有 Linux 安全租约与清理恢复

owner 为同一 namespace 保留私有 full-mask mark 和 client／server reqid，并检查占用冲突；
文件锁与无密钥账本保留归属。TC 使用独占新建的 clsact 和最多八组精确 local／P-CSCF
地址的 ingress flower，仅匹配 ESP，将完整 mark 写入 SA 查找前的 skb。IPv4／IPv6
分别匹配，不能借用已有 clsact、覆盖 foreign filter，或以 matchall 替代生产规则。

TC 和 XFRM 均通过持有的 namespace FD 创建显式 raw NETLINK_ROUTE／NETLINK_XFRM socket，
核验 namespace cookie、socket 身份及关闭栅栏；不依赖可能退回当前 namespace 的全局
handle／子进程。跨 namespace 创建 socket 时显式核验原线程 namespace 恢复，失败不会
把该线程交回 Go 线程池。state／policy 使用完整 mark、预留 reqid 和精确双方地址。

新增对象先写意图，再单次发送 CREATE|EXCL；ACK 后才以严格已知 schema 的原始静态
fingerprint 记录归属。删除前重新核对完整 fingerprint，policy 还核对内核分配的 index。
未知属性、foreign 对象、意外 mark writer 或 ACK 结果未知均保守隔离，不 replace／flush。
WAL 不保存 CK／IK 或原始含密钥安装报文，匹配的 readback 不能替代缺失的成功 ACK。

安装 worker 使用独立密钥副本；调用者快速取消返回后，可以擦除原始缓冲，但不能提前
擦除仍供该 worker 发送的报文。worker 在独立有界操作及必要的迟 ACK 收尾结束后，
统一擦除完整密钥缓冲；这些副本和原始 add payload 始终不进入持久日志。

`recoverKernelSecurity` 支持 **ACK-owned security 的 cleanup-only 恢复**：在原网络租约
已重新建立且身份可证明的前提下，重开安全 driver、核对保留项和账本，回收已确认归属的
TC／XFRM。它不重新安装 SA、不重新注册，也不是冷启动 PDN／网络重建。若账本存在
pending／unknown ACK，则保持 fence，不能靠内核对象“看起来相同”推断 adopt 或成功清理。

新 I/O 仍要求活动 PDN 来源证明；已有 ACK-owned 对象的清理则调用 `cleanupIdentity`，
只依赖持有的 namespace／netlink socket／原 sysfs anchor 身份，不要求 MM、SIM、PDN
仍连接，也不要求旧网卡名、UP 或 MTU。原设备 rename／Down 后仍可清理；只有严格证明
原 netdev 已删除，才退休其已由内核移除的 TC 账本。即便 ifindex 被新设备复用，也不对
新设备执行 TC 删除。无法证明时保留 fence，不把查不到旧名字当作设备已消失。

这些组件尚不构成完整模组生命周期／生产业务模式；真实固件、完整冷启动网络与 PDN
恢复、实卡多模组及信令／媒体联合验收仍未完成。

## 小区候选与新鲜性仍分开

新增独立 NAS 0x0043／0x13 解码器，从同一响应提取 PLMN／TAC／28-bit ECI／EARFCN／PCI，
保留两位／三位 MNC，按核对过的 TS 36.101 频段表区分 FDD／TDD。拒绝 reserved、SDL、
未知 overlay、扩展 EARFCN 及混合 RAT，不能拼接独立响应或从 IMSI 猜服务 PLMN。

独立解码器只提供候选值。新增 `NASServingCellSource` 则使用同一代次承载的自有 NAS／WDS
CID，核对 solicited 事务、有限查询时窗与查询期间已观察到的注册／小区／控制变化；只有
CID 精确释放 ACK、持久 pending fence 清除后，才返回 `NASCellQueryProof`。未知或迟到的
资源仍由原 owner 保留，不能因调用者取消而丢失。

这个 proof 是 **query-window proof，不是 continuous cell proof，也不是 MM 缓存证据**。
`QueryWindow` 是主机请求／响应区间，不是假造的模组采样时间；`ContinuousCellCoverage`
明确为 false，`Revalidate` 产生新的一次查询，不能证明两次之间未发生漏报的 A→B→A。
真实固件的 EARFCN 布局和查询新鲜性语义仍待验收；当前不自动生成 PANI、不设置
Established，也没有把该来源接到业务 REGISTER 开关。MM 缓存 observer 和 `Read` 仍保持
原有不可升格为新鲜 PANI 的边界。
消息布局依据 [libqmi NAS schema](https://github.com/linux-mobile-broadband/libqmi/blob/main/data/qmi-service-nas.json)。

## Windows 反向修复

PPP READY 的接口 index／可选 LUID 贯穿 probe、选择端点、注册 client、安全后端和媒体。
相同 IP 不再挑第一张网卡；owner 丢失、完成或注册期间被替换，不退回地址猜测。

WinDivert outbound 的 IfIdx 并不保证实际路由。每次发送现在核验未指定接口的系统最佳路由
确实属于 owner，同时持续检查接口 LUID，避免 index 复用。与强制接口的 route proof 同时
通过才发送。依据 [WinDivertSend 文档](https://reqrypt.org/windivert-doc.html#divert_send)。

这是**错承载 fail-closed**，不等于 Windows 已支持所有相同 IP／P-CSCF／端口的双模组；
全局 UDP 绑定与原始注入的路由限制仍需单独解决。未替换运行中的 VoWin。

## 验证记录

最终源码的测试和摘要统一记录在工作区
`publish/vocat-security-owner-20261024/verification.json`；前阶段边界验证保留在
`publish/vocat-owned-security-20261024/verification.json`。数字按顶层用例计算，不重复
加子用例；重复执行次数另标，不代表不同测试定义：

- Go 全仓库 1302 通过、2 跳过；Windows Host IMS race 十轮共 890 通过。
- Linux 原生 Host IMS race 156 通过、36 项显式环境 guard 跳过；owner／NAS 十轮
  260 通过、10 项 native-owner guard 跳过，该 owner 另在隔离 namespace 实跑。
- 隔离内核网络三轮 72、清理身份十轮 50、TC 十轮 60、scoped XFRM 三个独立
  namespace 9、双 owner 联合 3、required socket ESP 12，全部通过且无跳过。
- Linux IMS 消费者 203 通过、6 项内核 guard 跳过；imssec 7 通过，隔离数据路径
  三轮 138 通过且无跳过。这组消费者二进制未启 race，不能混称原生 race 用例。
- Linux amd64／arm64 构建及 Linux vet 通过，仅 amd64 在指定主机运行了版本命令。
  本轮 C# 未改也未重跑，262 份源码与前阶段基线一致，201 份共享镜像核对通过。

最终验收无失败。三处旧测试 fixture race 的失败日志、同步修复及中间冻结均保留，
不计入成功验收；生产逻辑未因修复这些测试而改变。完整账本最大支持用例为
20,993／32,768 字节，已通过 SQLite 写入／读回／解码／精确删除回归。

TC 和清理身份的独立记录分别见 `publish/vocat-security-tc-20261024/verification.json` 与
`publish/vocat-network-cleanup-20261024/verification.json`。NAS source 的单独查询／race／
fuzz 记录在 `publish/vocat-nas-source-20261024/`；解码器和共同计划的 instrumented fuzz
另见 `publish/vocat-serving-cell-nas-20261024/`、`publish/vocat-imssec-plan-20261024/`。
这些组件记录和前阶段历史数字不替代最终源码联合回归。隔离 ESP、合成 registrar 和
query-window 测试均不是运营商互通、固件新鲜性、实时 PANI 或实卡多模组成功证据。

## 后续组合接入

后续的 `PreparedAccess`、同查询 NAS/WDS 证据、有界查询年龄及单线路 `hostimsruntime`
接线见 [Host IMS Access](HOST_IMS_ACCESS.md)。该阶段新增内部组合 API，不改写本文已冻结的
历史验收数字；主应用模式、服务器分派、UI 与实卡/完整冷启动验收的边界以新文档为准。
