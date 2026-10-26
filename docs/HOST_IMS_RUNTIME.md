# Linux Host IMS：自有 WDS 与 P-CSCF 来源连接器

2026-10-23：新增 `NewCellularRuntimeSource(lease)`，连接已拥有的 MM 承载、
已绑定的 QMI 物理端点与进程自有 WDS CID。构造不打开设备、不分配 CID、不创建 PDN。
显式 `Read` 才进入查询；没有 Start／Stop Network、profile／autoconnect、RF、
内置 IMS 接管、SIM 切换或 SIP 注册操作。

**这是已实现并经过私有代理／拓扑夹具验证的生产连接器，不是已开放的 Host IMS 模式。**
尚未在真实 qmi-proxy／模组上确认“新自有 CID 绑定后读取现有 MM 承载”是否被该固件
支持；返回 OutOfCall 或未连接时明确失败，不借 MM 的 CID，也不偷偷 Start Network。
实际服务小区、SIP／ESP／RTP 接线、接口启动／RA／网络管理器协调、恢复与模式 UI
仍需完成。原 LICENSE 和所有产品授权限制不变。

## 自动端点与 mux 证明

- 沿用 MM `Ports` 自动选择及设备节点／物理 sysfs FD 锚定。
  当前连接器明确支持可证明的 **USB QMI**；不把 native MHI／PCIe 伪装为 HSUSB，
  其端点映射仍待另行实现与验证，不是最终放弃支持。
- QMI 控制节点必须属于实际 USB function；从该 function 的 `bInterfaceNumber`
  读取端点接口号，核对 USB subsystem，不从网卡名或设备序号猜数字。
- 复用承载核对 netdev → lower link → USB function，与控制端口是同一 function。
  qmi_wwan 从 `qmap/mux_id` 读取；rmnet 从原始 rtnetlink LINKINFO／INFO_DATA 读取，
  mux 必须为 1–254。不能用 profile index、AT PDP CID 或“第一台模组”代替。
- 显式 SinglePDN 不伪造 mux；直接接口仍须归属同一 USB function，已有 bearer
  所有者也仍会拒绝其他配置承载。该模式不是默认数据连接的偷用路径。
- 保持命名空间、netdev、USB function 和控制端点的句柄／元数据；核对 ifindex、
  iflink、名称、父接口、类型、MAC、物理代次及当前 MM 硬件／SIM／承载。
  查询前后变化即拒绝。这里只读拓扑，不把网卡 UP，不改 MTU／默认路由或 NM。
- rtnetlink 先检查完整对齐属性流，再解码。拒绝尾部碎片、重复 mux、错误类型、
  未支持属性及不适用的 byte-order 标志；不会用库忽略字段的行为宽松认领资源。

## 自有 CID 查询与清理

每个已配置地址族分配一个新的 WDS CID，严格核对 CTL 回显的 service＋CID：
端点／mux 绑定 → 仅该 CID 的 Set IP Family → connected 状态 → Current Settings
→ 再次 connected。两族都核验精确 APN、本地地址／前缀及已报告的网关／MTU，
不将 DNS、网关或 profile 当作 P-CSCF。

部分固件只在一族的 Current Settings 返回两族 P-CSCF 列表；只有两族各自的本地
承载均已证明后才合并这些列表。身份及网关不跨族借用。数字 P-CSCF 最多八个，
按已证明的可用地址族选择；仅有域名时保留解析结果，但尚无承载绑定 DNS，明确失败，
不让系统默认网络代发解析或 REGISTER。

使用已有可信本机 qmi-proxy 和同一进程 QMI 序列门，所有请求单次发送，不发 CTL Sync、
不重放分配／释放、不原始 QMI 回退。读取完成先确认释放每个自有 CID，再关闭代理、
拓扑与序列门，之后才交付 `RuntimeProof`。不是长期占着序列门阻塞后续 AKA。
WDS indication 会使这次读取证明失效，但不等于 CID 被撤销：仍可释放已知自有 CID。
释放阶段已观察到的 WDS 变化也会阻止交付证明。

共享 bearer 私有记录新增 `RuntimePending`／`RuntimeUncertain`。WDS 查询、UIM／AKA
和网络修改互斥，父 PDN 不能先行关闭。取消立即返回后台阶段的调用方，已发出请求
由独立观察者收尾，不继续查询第二族；不把取消当成远端没有执行。
前置只读检查与持久写入仍受各自 I/O 期限约束。读取前台／后台各二十秒，清理四秒，
这些不是对每个不可取消系统调用的硬实时保证。

分配或释放未知、端点／卡变化、清理失败时不交付证明，资源和持久屏障一起保留。
保留对象挂在应用级 bearer claim，而非只存在于调用方或自引用对象中，避免调用方
丢弃 provider 后被 GC 提前关掉未知会话；这个生命周期修正也补到 UIM 失败保留路径。
没有自动失效、重置模组、清空数据库或“按相同 APN 猜归属”的恢复路径。
临时查询 Busy 不会把尚未变动的网络 Close 永久标为不确定；查询结束后可重试清理。

## 与主机网络租约连接

`RuntimeProof` 只返回副本，不暴露代理／CID；格式化也不会递归打印私有身份对象。
Verify 会重新读取并比较同一租约、USB／mux、两族设置及 P-CSCF，而非永远相信缓存。
按集合规范化地址顺序和 APN 大小写，不因等价排序变化误判。

包内 `prepareProvenKernelNetwork` 将该证明接到上一阶段的网络租约：复用 Verify、
socket 绑定与新增媒体路径前重新核验。P-CSCF 改变后拒绝继续，新目标不能借旧路由；
原有已确认自有资源仍可正常清理。**这仍未接到 IMS 的实际 TCP／UDP、保护端口和 RTP
消费者**，也不实现“蜂窝已建立／已注册”的 UI 声明。

上述为本阶段边界；后续[网络消费者接线](HOST_IMS_DATAPATH.md) 已补 SIP／RTP 对同一
承载网络能力的调用、取消和路由生命周期。生产模式组装、新鲜小区证明及 XFRM 隔离
仍未完成，因此依然没有开放可启用的 Host IMS 模式。

## 验证

- Go 全仓库 1241 通过、2 跳过；Host IMS 72 个顶层用例 race 重复十轮，720 通过。
- Linux Host IMS 原生全套 100 通过、25 跳过。跳过为 23 个需显式隔离启用的内核
  场景及两个实系统只读探测；不是把它们当成实机成功。
- 独立 network＋mount namespace 的 23 个内核场景重复十轮，230 通过。
  另有 WDS／来源／拓扑 23 个场景重复十轮，230 通过，无跳过。
- rmnet 元数据 fuzz 十秒、双 worker，1325262 次执行、0 失败，含有效嵌套属性种子。
- Linux amd64／arm64 CGO=0 构建；amd64 版本命令运行，arm64 尚未实机运行。
  C# 镜像 201 文件一致，本轮未改 C# 或重新运行其全套。

Linux 测试仅部署在指定 `root@10.0.0.2` 的独立目录。QMI 使用真实私有 Unix socket／
SO_PEERCRED、合成 ModemManager／SIM／固件；拓扑使用临时设备节点、sysfs 和 netlink
元数据夹具。rmnet 原始属性有严格解析测试，但没有实际 rmnet 驱动链路或运营商承载。
没有访问真实 UICC、创建实际 PDN、实际 REGISTER、电话或短信，也未改现用 Asterisk、
主机默认路由或 Windows 部署。没有 WSL。

记录位于工作区 `publish/vocat-host-ims-runtime-20261023/verification.json`。
本轮无新增第三方依赖，不跨许可复制协议源码；只依据公开消息布局和行为独立实现。

依据：[libqmi WDS 消息定义](https://github.com/linux-mobile-broadband/libqmi/blob/main/data/qmi-service-wds.json)、
[ModemManager QMI bearer](https://github.com/linux-mobile-broadband/ModemManager/blob/main/src/mm-bearer-qmi.c)、
[QMI 端点来源](https://github.com/linux-mobile-broadband/ModemManager/blob/main/src/mm-port-qmi.c)。
