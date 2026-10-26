# Linux Host IMS：自有内核网络租约

2026-10-23：补上地址、独立策略路由表、socket 设备绑定与按通话引用的媒体主机路由。
这是可执行的真实 netlink 组件，不是配置预览；但构造入口仍为包内私有函数，
**未接入可启用的 Host IMS 线路**。后续已有[USB WDS／P-CSCF 来源连接器](HOST_IMS_RUNTIME.md)；
真实固件验证、SIP／RTP 消费者接线、
网络租约跨重启恢复和模式编排尚未完成，不能把隔离内核测试当成运营商注册或语音验收。

## 所有权与隔离

- 依附现有 `BearerLease`，核对 MM 服务代次、硬件、SIM、精确 APN、接口与地址。
  Runtime 的本地地址必须属于该承载；最多八个 P-CSCF。当前 Runtime 输入只验证
  结构及一致性；后续新增的 proven 构造入口另要求 WDS CID／mux 来源证明，不使用手填来源。
- 保持网络命名空间 FD、netlink socket 与 sysfs netdev 目录 FD；核对命名空间、
  ifindex、iflink、名称、父接口、MTU、MAC、类型及锚点元数据。生产路径还核对
  物理设备归属和复用链路；qmi_wwan 的 mux_id 只接受 1–254。
  后续 WDS 连接器可解析 USB rmnet 的原始 mux 元数据，实际驱动／固件联合验收仍待完成。
- 每条租约随机选取空的私有表 `0x40000000..0x4fffffff`，规则优先级 20000、
  协议标记 242；同时匹配本地源地址和输出接口。只加 P-CSCF／媒体的 /32 或 /128，
  不添加默认路由、不覆盖现存规则、不 flush 地址或其他软件的路由。
- 地址添加带 `IFA_F_NOPREFIXROUTE`，IPv6 等待 DAD，不用 NODAD 跳过检查。
  仅可复用同前缀、永久且已就绪的现存地址；借用地址从不删除。MTU 设置在自有
  主机路由上，不改共享物理接口。发现所选接口已有默认路由时拒绝开始。
- 使用实际带 source＋oif 的 FIB 查询核对所选表和接口。较高优先级的 VPN／黑洞
  规则若抢走路径，关闭该租约并仅回滚已确认自有资源，不修改 VPN。
- `BindSocket` 校验实际 socket 的 `SO_NETNS_COOKIE`、地址族及 TCP／UDP 类型，
  然后设置并回读 `SO_BINDTODEVICE`。仅接受该承载的本地地址，或已登记的信令／
  媒体目标；拒绝任意目的地、已有其他设备绑定及跨命名空间 socket。
  这里只验收了路由查找与绑定；还没有把这些钩子接到 IMS TCP／UDP、保护端口和 RTP。

当前要求自有接口和必要的 lower link 已 UP；不会悄悄启用物理父接口、修改
NetworkManager 配置或接管其默认 PDN。接口启动、RA／SLAAC 与网络管理器共存
仍属于后续生产承载编排工作，不是永久要求用户手动配置。
socket 命名空间检查依赖内核支持 `SO_NETNS_COOKIE`，不支持时明确拒绝，不降级绕过。

## 持久记录与取消

写入每一步意图后才发送内核修改；获得 ACK 后先记录已确认添加，再保存完整回读快照。
地址、规则和路由倒序清理，删除前再比对快照。规则动作与路由的原始属性也单独读取：
netlink v1.3.1 的类型解码会漏掉规则动作和部分路由属性，不能仅靠结构体相等判断。
未知选择器／属性拒绝；路由使用计数缓存仅在无到期时间时排除在稳定指纹之外。
这不提供对恶意特权进程“完全相同对象替换”的绝对 ABA 防御。

内核负面确认与丢失 ACK 不同：已知拒绝可撤销本次已确认资源；未知结果不凭“看起来
相同”认领或删除，保留记录、句柄和父 PDN 屏障。存储失败、接口代次变化或外部替换
同样停止继续操作。旧租约不得释放新代次的资源。

修改进入后台后，调用方取消可立即返回；前置只读探测仍受单次 I/O 超时约束。
已经发出的步骤由后台继续观察，确认后清理，不继续创建后续
地址／规则／路由。netlink socket 单次收发超时两秒，安装后台上下文二十秒，后续操作／
清理十五秒；这不是对每个不可取消系统调用的硬实时保证。后台未结束时仍持串行门。
网络变更与 UIM／AKA 共享持久状态门，避免鉴权和网络修改并行穿透。

媒体路由最多八个目标、三十二个引用；同一目标共享路由，最后一个引用关闭时才删除。
P-CSCF 主机路由始终由信令租约持有，不能因媒体结束而删除。PDN 只有在全部自有网络
资源已知清理完毕后才可关闭。包含 128 个挑战摘要、20 个网络对象及删除意图的压力
原阶段样例占 14694／16384 字节；加入 Runtime 状态字段后为 14742 字节，未放大存储上限。

**本阶段没有网络租约跨重启恢复入口。** 有 Network 记录时，旧 bearer 的 Recover
也拒绝贸然断开；未知记录不自动失效、不按 APN 猜归属、不提供“清空记录重拨”。
生产模式必须在补齐可验证的恢复／退出协调后再开放，不能更换空数据库绕过屏障。

## 验证与未完成项

所有 Linux 内核实验仅在用户指定 `root@10.0.0.2` 的独立 network＋mount namespace：

```sh
unshare -n -m -- sh -c 'mount --make-rprivate / && mount -t sysfs sysfs /sys && VOCAT_NETNS_TEST=1 /path/to/hostims-linux-amd64.test -test.v -test.run=TestKernelNetwork'
```

必须在私有 mount namespace 内先隔离传播，再重挂 sysfs；单独 `unshare -n` 会保留
旧 sysfs 视图。测试还拒绝在 PID 1 的网络命名空间修改内核；测试用的虚拟接口许可
只存在于私有构造参数，不是生产环境变量旁路。

21 个顶层内核场景重复十轮，210 次通过，包括双栈、网关、两台合成模组地址重叠、
媒体引用、取消、ACK／存储失败、VPN 策略、规则动作及 IPv6 路由属性被替换、接口
变化、跨命名空间 socket 和默认路由保留。它们使用真实内核和合成 MM／接口，
没有实际模组 PDN、运营商 REGISTER、电话、短信、真实数据面音频或双模组实机验收。
现用 Asterisk、系统服务和主机默认路由未改，未使用 WSL。

完整记录：工作区 `publish/vocat-host-ims-network-20261023/verification.json`。
新依赖 netlink v1.3.1／netns v0.0.5 的 Apache-2.0 许可证随现有 third_party 目录
打包；Docker 构建和发行工作流本轮未运行。原 LICENSE 与所有授权检查保留。

后续已有[USB WDS 自有 CID／端点／mux 与承载证明](HOST_IMS_RUNTIME.md)；仍须完成
真实固件联合验证、native 端点、DNS-only 情况、LTE 小区读取、模组内置 IMS 协调、SIP／ESP／RTP 网络钩子、
网络恢复、逐模组业务／模式 UI，以及真实运营商注册、续期、短信和双向语音。
Rust 仅作所有权行为参考，Go 代码独立编写，未跨许可复制协议实现。

接口依据：[Linux rtnetlink](https://www.kernel.org/doc/html/latest/networking/netlink_spec/rt-route.html)、
[netlink v1.3.1](https://github.com/vishvananda/netlink/tree/v1.3.1)、
[ModemManager QMI bearer](https://github.com/linux-mobile-broadband/ModemManager/blob/main/src/mm-bearer-qmi.c)、
[qmi_wwan](https://github.com/torvalds/linux/blob/master/drivers/net/usb/qmi_wwan.c)。
