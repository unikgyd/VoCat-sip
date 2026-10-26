# AMR-NB 媒体、默认构建与发布

Linux amd64、arm64、386、arm 的默认源码构建已接入真实 AMR-NB 双向编解码、RTP 收发
及浏览器 PCM 桥接，不再要求开发标签 `vocat_native_amr`。不支持的平台以及显式
`vocat_static` 构建保留 G.711，不宣告 AMR。不能将回环或本地编解码自检当作运营商通话验收。

这是当前工作区及修改后的发布工作流，不代表上游已发布的二进制／GHCR 镜像已经升级。
本阶段实际软件证据与平台边界见[阶段记录](../../publish/vocat-amr-release-20261024/README.md)。

## 编解码与资源归属

后端以 purego 0.11.2 调用主机的 `libopencore-amrnb.so.0`，不启动外部音频转码进程，
也不复制 Windows DLL。每通电话独立持有编码器和解码器，原生调用与关闭互斥；缺库、
缺少符号或状态初始化失败时不宣告 AMR，G.711 仍可用。

OpenCORE 的编码包装函数虽然接收 const 指针，内部会将语音转为可变缓冲区并进行
预处理。因此编码和解码使用独立输入副本，不能修改调用者持有的 PCM 或语音帧。
这项修复也已应用到 VoWin，避免同一缓冲区重复编码后音量衰减。

PCM 为 8000 Hz、16 位、单声道，每帧 160 样本。支持 AMR-NB 模式 0 至 7，以及入站
SID／NO_DATA；不宣告没有双向编码能力的 AMR-WB。每次 RTP 收包最多 16 帧，交给 PCM
消费者时按最多 160 样本拆分，避免多帧报文放大队列的音频积压。

## 协商与 RTP 格式

接收 Octet-Aligned 和 Bandwidth-Efficient 两种单声道格式，先完整验证 TOC、帧类型、
语音长度和整包边界，再交给原生库。支持多帧，不把截断帧补零为正常语音；按标准忽略
接收侧的保留和填充位，但拒绝多余整字节。发送端使用精确 speech-bit 数，不能将存储
帧的逐字节填充塞入带宽高效格式。

发送遵守 mode-set，处理有效且在集合中的 CMR 请求，并遵守 mode-change-period 与
neighbor 约束。CRC、交织及 robust sorting 暂不协商；对端只能提供这些格式时失败，
不假装成普通 AMR。主动 offer 使用 PT 102 和带宽高效格式，answer 不能擅自改 PT 或
对齐方式。被叫 answer 保留所选对齐方式与模式集合。

SDP 仅从选中的有效 audio block 取地址、映射与 fmtp，视频或后续 audio 不覆盖本通
媒体。对端 RTP 包通过已协商编码格式校验后才学习对称 RTP 端口，畸形包不能改变路由。

## 本地媒体自检

```sh
vocat media-check --json
vocat media-check --require-amr --json
```

该命令不打开模组、SIM、数据库、网络 socket、麦克风或扬声器。它使用合成 PCM 实际
编码和解码，并检查输入不变、帧格式、输出长度及非零能量；不只是检查文件是否存在。
普通模式即使无 AMR 也返回诊断；`--require-amr` 在不可用时退出非零，JSON 仍独立输出。

`runtime_flavor` 为 `glibc-amr` 或 `static`；`amr_nb` 的 `code` 区分 `ready`、
`dependency_unavailable`、`self_test_failed` 和 `not_built`。AMR-WB 仍为不可用。
管理会话的 `/api/system/info` 返回同一能力信息，系统信息卡片用状态灯显示；缺依赖的
说明默认收起。这里的“编解码可用”不表示 IMS 已注册或真实通话双向可闻。

## 构建、容器与更新

受支持的普通 Linux 发布使用 glibc 构建环境及 purego 动态加载器，即使 `CGO_ENABLED=0`
也不是完全静态。musl 构建环境须显式选择 `vocat_static`，不能仅靠关闭 Cgo 判断兼容性。
运行时需匹配 CPU 架构的 OpenCORE-AMR-NB；Debian／Ubuntu 包名为
[`libopencore-amrnb0`](https://packages.debian.org/bookworm/libopencore-amrnb0)。

```sh
sh scripts/build-linux.sh ./vocat native
# musl / OpenWrt 或明确不需要原生 AMR 的静态构建：
sh scripts/build-linux.sh ./vocat-static static
# armv7 原生版明确使用 armhf 加载器，不修改宿主机 /lib：
GOARCH=arm GOARM=7 sh scripts/build-linux.sh ./vocat-armv7 native
```

修改后的发布流程对每个现有 Linux 架构提供两种资产：`vocat-linux-<arch>` 为 glibc-AMR，
`vocat-linux-<arch>-static` 为静态 G.711。两者均有 SHA-256 校验。安装器只在明确识别
glibc 时选择原生版；未知 libc 选择静态版，并在替换前验证下载程序能启动。它只提示缺失
AMR 库，不自动给宿主机安装新的 AMR 包。新版本静态程序自更新只选择 `-static` 资产，
缺少对应类型时拒绝，不悄悄切换到 glibc。

旧版程序不知道新资产类型；musl 用户首次迁移应使用新版安装器或手动选静态资产，
不要把原生版重命名后覆盖。现有自更新的可执行验证仍在替换前生效。

Docker 的 Go 构建器与运行层均为 glibc 环境，运行层安装 AMR、QMI、PC/SC 包，
并保留本地依赖许可证。镜像构建时强制执行 AMR 自检；诊断命令走标准入口时不启动 pcscd。
不能用 Alpine 构建器生成的 musl 解释器路径搭配 Debian 运行层，否则程序连自检都无法启动。

## 开发验证

在具备依赖的开发工作区构建 Linux amd64 测试程序：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c ./internal/vowifi/ims -o ims-amr-linux.test
VOCAT_REQUIRE_AMR_TEST=1 ./ims-amr-linux.test -test.v -test.timeout=90s
```

`VOCAT_REQUIRE_AMR_TEST=1` 将缺库视为失败，不能把跳过的原生测试算作成功。XFRM 测试
仍需单独的隔离网络命名空间；不要在生产主命名空间设置 `VOCAT_NETNS_TEST=1`。

amd64 原生运行与其他架构的构建／模拟器运行边界分别记录在阶段记录中；模拟器通过不代表
真实 ARM／32 位硬件验收。musl 原生 AMR 后端、AMR-WB 编码、实卡通话和实际听感仍待完成。

测试使用合成音频，覆盖八种模式、两个 RTP 格式的实际双向 UDP 回环、输入不变、关闭
后拒绝复用、缺库降级，以及与 VoWin 共用的 32 条独立比特样例。Linux 原生结果还与
独立 C 程序直接调用同一库的八种模式逐项比对；不读取真实卡或录制真实通话。

依据为 [RFC 4867](https://www.rfc-editor.org/rfc/rfc4867.html)、
[OpenCORE 编码接口](https://github.com/BelledonneCommunications/opencore-amr/blob/master/amrnb/interf_enc.h)
及其[包装入口](https://github.com/BelledonneCommunications/opencore-amr/blob/master/amrnb/wrapper.cpp)。
purego 和 Go 派生部分的许可文本保留在 LICENSES，VoCat 原许可与授权检查不变。
