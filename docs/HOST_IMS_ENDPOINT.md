# Linux 蜂窝 QMI 端点与 UIM 生产连接器

2026-10-23：新增 `NewCellularSIMProvider(lease)`，把上一阶段的卡内身份／AKA 事务
连接到 Linux QMI 端点与 qmi-proxy。构造不打开设备、不分配 CID；只有显式读卡或
鉴权操作才激活连接。**尚未接入逐模组业务运行时／模式 UI，也未做真实卡 UIM／AKA
或运营商 REGISTER 验收**，不能据此认为 Linux Host IMS 已可启用。

## 自动发现与绑定

- 从当前 ModemManager 独有服务实例的 `Ports: a(su)` 读取类型 6 的 QMI 端口。
  唯一候选直接使用；多个候选只有 `PrimaryPort` 明确命中其中一个才选择。
  不猜 `cdc-wdm0`，不把 MBIM、AT、网络接口或未列出的端口当 QMI。
- 端口名规范化到单层 `/dev/<name>`，拒绝路径穿越、外部路径及重复／畸形端口数据。
  属性读取前后还核对槽位、SIM 和端口，避免拼接服务重启／换卡前后的状态。
- O_PATH 保持设备节点及物理／字符设备 sysfs 目录的句柄；核对设备号、inode、
  模式、ctime 和有效链接数。`/sys/dev/char/<major>:<minor>` 必须属于所选物理设备，
  class 必须是 usbmisc／wwan／mhi。重插、节点替换、符号链接和错误归属会使验证失效。
- `ProbeQMIEndpoint` 只做这些元数据核验，不打开驱动做 I/O，不连代理，不发 QMI，
  也不读取 UICC。当前真实主机探测确认 1 个选定端点，协议操作数为零。

## qmi-proxy 与自有 CID

本连接器要求**已运行的本机 qmi-proxy**，使用标准抽象 Unix socket `qmi-proxy`。
通过 SO_PEERCRED 核对连接对端为 root 或当前运行用户，不连接远程代理，也不提供
任意代理地址／命令配置。缺少或不受信任时报告独立的 `ErrQMIProxy`，不误报 SIM 坏卡。
目前不自动安装、启动代理或修改系统服务；部署时须准备对应发行版的 libqmi 组件及权限。

已有 QMI SDK 的连接器可能自动 CTL Sync，CID 分配器会在传输错误后重试，解码器
也允许部分宽松布局。因此本路径只复用既有 QMI 数据类型，独立实现小型串行代理传输：

- 仅代理设备 open → 单次 CTL 分配 UIM CID → 自有 UIM 事务 → 单次释放原 CID。
  不发 CTL Sync，不查询版本、不 Reset、不切 RF／profile／SIM，不分配其他服务 CID。
- 完整校验 QMUX／CTL／服务头、事务号、CID、消息号、响应／指示标志、TLV 精确长度
  与重复字段。分配／释放成功还须精确回显 UIM service 与 CID，不借用 MM 的 CID。
- 连接内请求串行，事务号不回绕；单请求最长五秒，取消中断阻塞读写，Close 有界等待。
  丢失／畸形结果不重发，关闭失败仍保留所有权；异步 CID revoke、Sync 或 UIM 变化
  使该连接失效，不自动重连、换应用或继续鉴权。
- 请求原始缓冲、失败响应和取消时已排队但未消费的 AKA 响应会擦除。
  生产诊断只交付静态错误，不包含私有驱动字符串、APDU 或身份。
- 与现有 QMI 操作共用进程内序列门。USB cdc-wdm 仅持门、不打开 raw 描述符；
  native WWAN 延续已有 keeper，防止最后一个描述符关闭后驱动移除节点。
  keeper 不发送 raw QMI，不是代理失败后的 raw 回退。跨进程 Host IMS 仍受 bearer
  的活进程锁／SQLite 记录约束；这不等于锁住其他软件或 MM 的全部行为。

分配或释放结果未知时，连接、端点锚和序列门的所有权与 `SIMUncertain` 一起保留，
拒绝后续自动操作。调用方取消后仍观察已发出的分配；若迟到成功得到确认，可只释放
当次自有 CID 并清理；前台不等待整段事务。没有“强制忘记并重拨”入口。

## 验收与未完成项

在用户指定的 `root@10.0.0.2` 中，用私有 Unix 代理实现完成：生产 opener、真实 socket
与 SO_PEERCRED、严格线缆帧、UIM 卡内身份、ISIM AKA 和逐项清理。另验证 CID 分配／
释放、AUTH、通道关闭的丢回复，不重试、不误释放、不交付不可靠密钥，以及取消后
迟到 CID 的确认收尾。端点测试使用临时字符设备节点和私有 sysfs 树，D-Bus 测试使用
私有假 ModemManager。**这不是已在真实 qmi-proxy 二进制或真实 UICC 上验收的证据。**

临时字符设备测试需要 CAP_MKNOD；普通无权限 CI 可跳过这些专用用例。验收主机设置
`VOCAT_QMI_NODE_TESTS_REQUIRED=1`，缺少能力直接失败，不允许跳过充数。
真实系统只读探测须显式设置 `VOCAT_QMI_ENDPOINT_PROBE=1`；不自动启用实卡测试。

仍需接入业务授权／模式所有者、自有主机地址与 P-CSCF／媒体路由、真实小区信息和
ESP 注册，再接模式 UI，最后进行合规实卡续期、短信、通话和双向音频验收。
当前没有真实 REGISTER、呼叫、短信、Asterisk 修改、默认路由修改、服务安装或
Windows 程序替换，不使用 WSL。原 LICENSE／授权校验和已有依赖版本保持不变。

本轮结果：工作区 `publish/vocat-host-ims-endpoint-20261023/verification.json`。
接口布局依据 [ModemManager Modem 接口](https://github.com/linux-mobile-broadband/ModemManager/blob/main/introspection/org.freedesktop.ModemManager1.Modem.xml)、
[端口类型枚举](https://github.com/linux-mobile-broadband/ModemManager/blob/main/include/ModemManager-enums.h)、
[libqmi CTL 消息定义](https://github.com/linux-mobile-broadband/libqmi/blob/main/data/qmi-service-ctl.json)。
