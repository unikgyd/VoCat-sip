# Linux Host IMS：信令与媒体的受控网络边界

2026-10-24。上一阶段的 [WDS 来源证明](HOST_IMS_RUNTIME.md) 和
[内核网络租约](HOST_IMS_NETWORK.md) 现在接入实际 IMS 消费者。
这不是已启用的 Host IMS 业务模式，也没有真实模组注册、电话或短信验收。
原 LICENSE 和地区／设备／SIM／评估授权检查均不变。

## 信令 socket

`CellularIMSBearer` 必须同时实现同一承载的 `CellularIMSNetwork` 与
`CellularIMSSecurity`，不能只提供 Established 布尔值和地址。安全能力 gate 在任何
网络操作之前执行。初始 SIP TCP／UDP、IPsec 激活后的重连、protected TCP listener／
UDP socket 都调用同一 owner 的网络能力，同时明确绑定分配的本地 IP；protected
socket 还在 bind／listen 前安装 required ESP socket policy，accepted TCP 也不能绕过。
默认保护端口的临时可用性探测也走相同绑定，不只覆盖手工指定端口。
VoWiFi 保持原有隧道路径，不套用蜂窝承载。

`ControlContext` 按 Go 的优先级组合原 hook，保留受保护 TCP 的 MSS 设置。
绑定失败返回脱敏错误，不继续连接、AKA 或 REGISTER。accepted TCP 还核验
本地 IP、server port 和同一网络能力。入站注册与关闭快照共用停止栅栏，
避免迟到 accepted socket 逃过清理，导致 `receiveDone.Wait` 永久等待。
媒体关闭移到 callMu 外。

Linux 原生测试还发现 Go 的默认 MPTCP listener 会在 Control 失败后回退 TCP。
蜂窝消费者现明确关闭 MPTCP；内核租约还核验 `SO_PROTOCOL`，不把任何 stream socket
都认作 TCP。保留 VoWiFi 原有行为，不用全局 GODEBUG 改系统或其他线路。

## RTP 路由与通话事务

RTP socket 同样绑定精确本地 IP 和承载设备。183 early media、最终 200、初始入站
offer、延迟 offer 的 ACK、UPDATE／re-INVITE 都通过同一路径：先取得目标 route reference，
再在发送锁内原子交换 remote＋route；旧引用锁外释放。失败不发布新目标，换 IP／端口
也清掉原对端的部分 PCM 缓冲。不会在每个 RTP 包上重新查询 WDS。

蜂窝初始 INVITE 先建立振铃记录，耗时 socket／route 准备延后到 AnswerCall。
等待期间不持 replyMu；CANCEL／本机挂断先取消媒体生命周期、关闭 socket，
迟到 acquisition 的引用进入清理，不会把已结束通话复活。
入站后续 SDP 由每通话单 worker 和四项有界队列处理，不堵住 SIP 接收循环。

重协商验证完整 dialog 身份及递增 CSeq；旧事务不能把新 SDP 切回旧目标。
相同事务的重传复用响应，不再次申请路由；pending responders 最多四个、完成缓存最多八项。
重协商的 CANCEL 与媒体提交共用原子仲裁：未提交时 200 CANCEL＋487 INVITE，
保留原活动通话与旧 route；提交后只确认 CANCEL、不回滚。
可取消地等待 RTP writer 也在事务栅栏外，栅栏内不等待路由、UDP 或编解码 I/O。

每媒体最多保留 32 个待清理引用；只有一个十五秒有界清理 pass，不无限创建重试 goroutine。
失败引用仍归属原 owner，通话历史不淘汰它。Session.Close 在锁外用独立、共享十五秒预算
调用 Drain，失败返回脱敏错误；重复 Close 或 `DrainCellularMedia(ctx)` 可重试。
并发第二次 Close 不能在第一次尚未完成时返回假成功。
最终 PDN owner 也必须保留并清理自己的资源账本，不能把丢弃 Session 当成释放成功。

## ESP 回复策略

原 receive filter 允许同一 P-CSCF 从临时端口发 TCP／UDP，但 server outbound
原来仅 TCP、固定 port-c。现同时覆盖 TCP／UDP：精确双方 IP、UE port-s、
对端端口 wildcard，必须使用 server-pair ESP SA；没有放宽到其他 P-CSCF。
本阶段安装及清理同步为四条 state、七条 policy；后续共同计划又补客户端 UDP 入站，
现为四条 state、八条 policy，见[同承载安全边界](HOST_IMS_OWNED_SECURITY.md)。

真实隔离内核测试观察到：UDP／TCP 发向协商端口及临时端口时，ESP 计数均增长。
流量只在新 namespace 的 TEST-NET veth 内发送，不是运营商互通证据。

蜂窝路径已禁止回落到默认的全局 `ip xfrm` installer，SA 由同一 owner 安装；非蜂窝的
既有 VoWiFi installer／隧道行为不因这项 gate 改变。新增的私有 `kernelSecurityLease`
以独占 TC flower、namespace-pinned raw NETLINK_XFRM、full-mask mark／预留 reqid 和
required socket policy 组成安全边界，具体归属／WAL 约束见[同承载安全边界](HOST_IMS_OWNED_SECURITY.md)。
`prepareKernelSecurity` 是可供后续接线的私有生产组件，不提供 Established 证据，也不等于
完整 Host IMS 模组模式已经开放。不能仅因 socket 绑设备就声称多模组 XFRM 已安全隔离。

## 小区缓存不能生成新鲜 PANI

新增真实 MM `GetLocation` 只读连接器，沿用同一 backend，核对唯一服务 owner、
SIM／硬件／承载、LTE RAT、注册状态、OperatorCode 和前后位置快照。保留两位／
三位 MNC，严格校验 LTE TAC／28-bit ECI，不从 IMSI 猜位置，不启用定位或改变 RF。

`ObserveCached` 仅供诊断。MM Location 没有样本时间、cell epoch 或 FDD／TDD，
两次相等读取也不能排除未观察到的 A→B→A。因此 `Read` 仍返回零 LTECell 和
`ErrServingCellUnavailable`，不把缓存升格为 PANI／Established。

另有同一承载代次、自有 NAS／WDS CID 的 `NASServingCellSource`，可返回有 solicited
时限和已观察变化检查的 `NASCellQueryProof`，并等待精确释放 ACK 后才交付。但它只证明
主机的一次 query window，不承诺后来 SIP 发送时仍在原小区；没有模组样本时间或完整
连续事件覆盖。它不是 MM 缓存，也不是 continuous cell proof，重新查询相等仍不能排除
漏报的 A→B→A。固件布局／新鲜性验收和实际 PANI 接线仍未完成，不自动设置 Established。

## Windows 反向补齐

参照 Go 取消观察者的边界，C# 复现了 owner 先 ADD 路由、后 ACK 时，取消使客户端
丢 pending、遗漏路由的问题。现对 ADD 发出后的取消、超时、错误 ACK、复核失败
立即设 Lost 栅栏，后台中止自有 owner；不等待模组恢复，不让迟 ACK 恢复会话。
明确 REJECT 和发送前取消不错误断开健康承载。新增四条确定性 fake-owner 回归，
并同步 C# 来源镜像，没有跨许可复制协议代码。

## 验证与剩余工作

信令测试覆盖合成 registrar／fake IPsec 注册中的各 socket hook、失败时禁止发送、
默认端口探测、accepted TCP、关闭竞态，以及 Linux 私有 namespace 的实际双栈
设备／namespace cookie／源地址／TCP＋UDP 收发／MSS／RTP 绑定核验。合成 registrar 不是
运营商 REGISTER；native socket 测试能力不冒充真实模组。

安全 owner 已有 ACK-owned TC／XFRM 的 cleanup-only 恢复入口，unknown ACK 仍保留 fence，
不通过匹配对象推断 adopt。`cleanupIdentity` 不要求 PDN 仍连接，保留 namespace／socket／
原设备 anchor 的归属检查；rename／Down 可清理，原设备删除后只退休其 TC 账本，绝不删
ifindex 复用的新设备。这不构成完整冷启动 PDN／网络重建。

真实固件 WDS 与 NAS 查询语义、可用于 PANI 的连续新鲜性边界、完整网络跨重启恢复、
模组 IMS 接管／恢复、逐模组模式／UI、RTCP 和实卡联合媒体仍需完成。没有改现用 Asterisk、
替换运行中的 Windows 程序或用 WSL。

记录：工作区 `publish/vocat-host-ims-datapath-20261024/verification.json`。
Linux 测试仅在 `root@10.0.0.2` 独立构建目录及隔离命名空间运行。

本阶段历史冻结源码验证结果（顶层用例，不重复统计子项；不覆盖后续 TC／scoped XFRM／
安全 owner 改动，本轮统一冻结验证及数字另见上述安全边界）：

- Windows Go 全仓库 1278 通过、2 跳过、0 失败。
- IMS race 192 项×10＝1920 通过；Host IMS race 79 项×10＝790 通过。
- Linux 原生 IMS 196 通过、5 跳过；Host IMS 112 通过、26 跳过。
  IMS 跳过为需显式隔离启用的两项 socket／三项 XFRM；Host IMS 跳过为
  24 项隔离内核场景及两项实系统只读探针，不算实卡成功。
- 独立 network＋mount namespace：24 项内核场景×10＝240 通过；
  信令／RTP／事务／XFRM 等46项×10＝460 通过，无跳过。包含真实 ESP 计数增长。
- 服务小区 parser 带覆盖反馈 fuzz 十秒、双 worker，49776 次执行，0 失败。
- C# PPP lease／SA plan／protected response 定向43项通过；未重跑完整桌面测试套件。
- Linux amd64／arm64 CGO=0 构建及 Linux vet 通过；amd64 运行 version，arm64 未实机运行。
  C# 201 文件镜像一致；不代表 Docker／正式发布流水线验收。

`build-source-sha256.json` 冻结105个协议来源文件，最终审计会拒绝测试后变动。
初次 Linux 测试发现 MPTCP 回退重复 socket；中间态日志也保留，不用它们替代最终成功记录。

接口依据：[MM Location 文档](https://www.freedesktop.org/software/ModemManager/doc/latest/ModemManager/gdbus-org.freedesktop.ModemManager1.Modem.Location.html)、
[MM Location 实现](https://github.com/linux-mobile-broadband/ModemManager/blob/main/src/mm-iface-modem-location.c)。
