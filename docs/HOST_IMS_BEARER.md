# Host IMS 逐模组承载所有者

`internal/hostims` 新增真实 ModemManager D-Bus 后端及持久化 `BearerPool`。
这不是 UI 开关，也不是已完成的 Host IMS 注册服务。现有应用仍未创建该所有者；
没有自动切换模组、创建实卡承载或启动新服务。Windows 协议核心本阶段未修改。

## 已落地的约束

- 每个选定模组独立操作门与租约，按设备身份分开管理。库存读取只用设备字段，
  另一台模组没有 SIM 不会阻塞健康线路；重复设备身份拒绝选择。
- 选择绑定当前 D-Bus 独有服务名、总线实例、系统启动标识、硬件路径／节点、
  设备标识、SIM 路径、槽位、ICCID 与 IMSI。记录只保存绑定摘要，不保存明文卡身份。
  读写都指向当次独有服务名，不随 well-known name 转移到重启后的另一个进程。
- 默认只创建 `apn=ims`、`apn-type=IMS`、强制 multiplex 的 bearer，不借用或删除
  同 APN／IMS 类型的既有连接。显式 `SinglePDN` 模式拒绝任何其他已配置 bearer；
  不自动停 NetworkManager、断默认数据、修改 profile、开关射频或重启基带。
- 按 ModemManager 支持的地址族选择 IPv4v6／IPv6／IPv4；单次失败不自动再发另一轮。
  当前只接受可用的 static 配置，PPP／DHCP／SLAAC 尚未实现，不能当作地址已就绪。
  **地址不支持不等于所有权失效**：阻止交付配置，但仍可清理已证实属于自己的 bearer。
- 创建者必须提供授权回调，缺失回调直接拒绝；未来业务接入须保留现有地区、设备、
  SIM、模式互斥等检查。测试中的允许回调仅适用于合成设备，不能代替产品授权。

## 所有权、超时与恢复

进度顺序为 `reserved → created → connected → active → disconnected → deleted`。
每次 Create／Connect／Disconnect／Delete **先**用 SQLite compare-and-swap 写入意图，
成功确认后才推进阶段。新建请求不是普通 upsert；旧记录或其他代次不能覆盖、删除新记录。
没有 TTL、超时抢占、自动删除冲突 bearer 或按相同 APN 猜测所有权的路径。

同机所有生产实例使用 `/run/lock/vocat-host-ims/` 下的独占文件锁；目录须属于运行用户、
权限 `0700`，锁文件 `0600`、单硬链接、非符号链接。不删除锁文件，避免另一个 inode
绕开现有锁。进程活着且持锁时，其他实例不能恢复／接管。生产实例应共用同一持久数据库，
不能在结果未知时换一份空数据库重新启动。
容器还必须与其他 VoCat 实例共享该锁目录和数据库，并使用相容的文件所有者；
各自私有的容器 `/run` 不构成同机互斥。接入宿主 ModemManager 时还需正确开放系统总线
及权限，不能在容器里再启动另一个管理器争抢模组。当前 Docker 配置未自动完成这套接入。

调用方取消不等于 ModemManager 停止执行：后台保留最长 60 秒的响应观察，前台不等待
整段操作。待决或结果未知期间，不重试、不叠加断开／删除；确认迟到结果后，只能显式
`Recover` 清理原路径。丢失 Create 回复、错误回复或仍待决的意图不猜测恢复。
存储不可用也关闭新操作；记录不会因程序退出或时间推移而失效。

进程重启后，须先取得活进程锁，再检查完整绑定与原有属性，才允许显式清理已确认路径。
只写入 reserved、尚未发出任何修改的记录，可以不触碰模组清除；已连接线路不会自动
交给新调用者。旧租约不允许释放后续新代次。权限失败、身份／服务变化或不确定结果
继续保留记录，当前没有“强制忘记并重拨”按钮。

这里的进程锁只约束 VoCat 实例，不能锁住其他软件的所有意图。每次操作还会核对 MM
库存／属性；kernel 地址、路由、接口归属与网络管理器策略是下一层的独立验证要求。
调用者必须先停止 IMS／媒体和自有主机网络资源，再释放 bearer，不能反转顺序。

## 已完成的验收

- 合成双模组并行、取消／迟到创建／连接／删除、失败不重试、SIM／设备／服务变化、
  外部 bearer 保留、旧租约、持久意图失败、授权撤回与配置失败的安全清理。
- 同一 SQLite 文件的多个连接竞争只有一个获胜；敏感记录及陈旧 CAS 拒绝验证通过。
- 在 `root@10.0.0.2` 启动私有 `dbus-daemon`，由假 ModemManager 导出真实接口。
  Go 后端通过真实 D-Bus、真实 SQLite 和真实进程锁完成创建至删除、显式恢复、
  服务丢失及不支持地址配置后的清理；另有独立子进程验证文件锁拒绝竞争。
- 系统总线上只运行只读探测，识别到一台模组和 SIM。**没有向真实 ModemManager
  发送 Create／Connect／Disconnect／Delete，也没有真实 IMS REGISTER、拨号或短信。**
- 初次 native 测试发现 `testing.TempDir` 的子目录权限不符合私有锁目录要求；改测试
  使用新建的 `0700` 专用子目录，未放宽生产权限检查。另保留地址配置拒绝的先失败用例。

普通 Linux 测试需安装 `dbus-daemon`；实系统探测另需显式设置
`VOCAT_MM_SYSTEM_PROBE=1`。所有 Linux 实测仅在指定主机，不使用 WSL。
本阶段未改现用 Asterisk，也未运行 Docker／GitHub 发布流程。

## 距离可启用的 Host IMS 仍缺什么

1. 将所有者接入逐模组业务编排、统一模式互斥、快速退出和恢复界面，配置真实授权回调。
2. [UIM／ISIM／AKA 事务层](HOST_IMS_SIM.md)已补齐卡内身份、串行通道和挑战防重；
   [生产 Linux QMI 端点 opener](HOST_IMS_ENDPOINT.md)后续已补齐，逐模组运行时接线及
   真实 UICC 验收仍待完成。
3. [主机地址／策略路由及媒体租约](HOST_IMS_NETWORK.md)已有真实隔离内核验证；
   后续已有[不混用 MM CID 的 USB WDS／P-CSCF 来源连接器](HOST_IMS_RUNTIME.md)，
   仍需真实固件支持验证、native 端点及业务接线。
   MM 的 IP 字典不提供 P-CSCF，不能用 DNS、gateway 或 profile index 冒充。
4. DAD、默认路由拒绝及自有资源倒序释放已覆盖；RA、接口启动／网络管理器共存、
   实际 SIP／RTP 接线和网络跨重启恢复仍待完成。存在网络记录时暂不允许 PDN Recover。
5. 模组内置 IMS 的受控接管／恢复及实际运营商注册、续期、双向语音和真实多模组验收。

完整记录位于工作区 `publish/vocat-host-ims-owner-20261023/verification.json`。
新增 `godbus/dbus/v5 v5.2.2`（BSD-2-Clause），许可证随 Docker／发行附件携带；
原 VoCat LICENSE 和已有授权机制未改。Rust 租约状态机保留作设计对照，Go 实现独立编写。

接口依据：[官方 Modem 接口](https://github.com/linux-mobile-broadband/ModemManager/blob/main/introspection/org.freedesktop.ModemManager1.Modem.xml)、
[官方 Bearer 接口](https://github.com/linux-mobile-broadband/ModemManager/blob/main/introspection/org.freedesktop.ModemManager1.Bearer.xml)、
[主机 IP 配置职责](https://modemmanager.org/docs/modemmanager/ip-connectivity-setup-in-lte-modems/)。
