# VoCat-sip 部署与运行手册

更新日期：2026-10-28。面向 Linux 运维；以下命令使用 Bash。命令中的路径和发布标识须替换为本次已核验的值。本手册不代表新版本或容器镜像已经发布，也不代表所有硬件组合已经通过验收。

推荐使用 **Docker Compose 三服务**：`vocat` 管理主进程、`vocat-data-owner` 普通数据所有者、`vocat-qmi-proxy` 独立共享 QMI 代理。原生 systemd 三服务是可选方案。两种方案选一种，同一设备不得同时运行两套部署。

QDC507 使用纯 **Host IMS**：主机持有承载、认证、SIP 对话和 RTP/AMR 媒体。必须显式选择 `host_ims`，不使用已废弃的 QDC507 ADB/D4/UAC 语音桥。普通数据所有者是普通 QMI 数据恢复组件，不替代 Host IMS 控制器。

## 1. 上线范围与当前限制

| 项目 | 可引用的证据与上线条件 |
| --- | --- |
| 纯 Host IMS 电话、短信及 SIP 桥 | 已有真实运营商验收；参阅 [Host IMS SIP](VERIFICATION_20261027_HOSTIMS_SIP.md)、[来电与清理](VERIFICATION_20261028_SIP_FOLLOWUP.md)。新环境仍须验收本机固件、SIM、音频和路由。 |
| Compose 普通数据代理／活动 owner 死亡 | IPv4、IPv6、双栈已按记录中的显式恢复配置通过；依赖匹配补丁版 ModemManager，不能外推到未配置恢复、未知／旧版记录或主机断电。见 [代理故障](VERIFICATION_20261028_COMPOSE_PROXY_ACTIVE.md)、[活动 owner 故障](VERIFICATION_20261028_COMPOSE_OWNER_ACTIVE.md)。 |
| ModemManager | 测试环境的发行版 `1.25.95-1ubuntu1` 曾 SIGSEGV；故障验收使用匹配修补版 `1.25.95`。尚不能宣称发行版崩溃已修复或任意发行版可替代。未核验依赖是生产上线阻断项。 |
| IPv6 冷启动 | 曾在故障注入之前发生 DNS／公网查询失败，完整模块维护重启后才恢复基线。该维护不计入自动恢复成功，冷启动可靠性仍需目标环境确认。 |
| 原生 systemd 新监督关系 | 已改为 `Wants`＋启动前 unit/socket 双检查；实际电信双栈活动 owner／proxy 强杀分别约 37／67 秒恢复，原其他服务不退出。见[原生监督验收](VERIFICATION_20261028_NATIVE_SUPERVISION.md)。仅覆盖记录中的 data-only 场景，不能外推到全部地址族／Host IMS 联合故障。 |
| 浏览器／VoWiFi | 物理浏览器音频由用户延期；当前 VoWiFi 只要求连接验收。不能把浏览器 API、录音或软电话音频结果写成浏览器麦克风／扬声器实测通过。 |

软件测试和其他历史报告仅适用于其冻结候选。部署前记录本次源码状态、产物 SHA-256、镜像 ID／digest、硬件／固件、内核与依赖版本，不能用旧报告替代本次产物核验。许可、运行时授权和运营商准入仍适用。

## 2. 主机前置检查

所有 Bash 代码块启用失败即停和私有文件权限；校验、停止或备份失败时，先解决失败再继续。安装脚本的 `--check-env` 会执行依赖安装及 PC/SC 服务操作，因此不作为只读检查命令。

1. 使用实际 Linux 主机；Docker Desktop 的虚拟机网络不能代替本手册要求的真实主机 PID、网络、USB 和内核环境。确认设备供电稳定、串口／QMI／WWAN 可见、系统时间与 DNS 正常。
2. Host IMS 需要主机运行 ModemManager 和系统 D-Bus；容器里的工具不能代替主机守护进程。VoWiFi 需要匹配运行内核的 XFRM/IPsec 支持，不能强装其他内核的模块。
3. 确认 `qmicli`、`qmi-proxy`、`ip` 可用；原生 AMR 需要 `libopencore-amrnb0`，原生 USB PCM 需要 `libasound2`。读卡器另需 `pcscd`、`libccid`；压缩提示音导入按需安装 FFmpeg。普通 WAV 导入不要求 FFmpeg。
4. 从已有管理员核实每台物理设备及当前 SIM 的归属。先正常关闭通话、自动任务、接入和普通数据，等待原所有者确认清理。禁止在 VoCat 持有承载时另开独立 QMI 客户端操作同一设备。
5. 检查已有 QMI proxy。新 launcher 会拒绝接管一个已服务的 proxy。若它属于其他应用或 ModemManager，先协调所有原客户端的维护窗口及正常退出，再安排独立 proxy 提前启动；不能杀死未知 proxy 来腾位置，也不能全局停用仍服务其他线路的 ModemManager。

```bash
set -euo pipefail
umask 077
uname -a
date -Is
command -v ip qmicli
find /usr/lib /usr/libexec -type f -name qmi-proxy -print 2>/dev/null
systemctl status ModemManager --no-pager
mmcli --version
mmcli -L
pgrep -a qmi-proxy || true
ls -l /dev/cdc-wdm* /dev/serial/by-id/ 2>/dev/null
ip link show
ip xfrm state
ip xfrm policy
ss -lntup
```

Debian／Ubuntu 的原生依赖示例（按目标发行版核实包名）：

```bash
set -euo pipefail
umask 077
sudo apt-get update
sudo apt-get install -y ca-certificates iproute2 libqmi-utils modemmanager \
  libopencore-amrnb0 libasound2 pcscd libccid
```

ModemManager 崩溃问题和最小补丁见 [Host IMS 部署说明](HOST_IMS_DEPLOYMENT.md#modemmanager-12595-dependency) 与 [补丁文件](patches/modemmanager-1.25.95-netlink-completion.patch)。只对匹配发行版源码和依赖构建、离线核验，再在维护窗口选择私有临时可执行文件；保留发行版二进制和服务加固。不要将本文示例视为修补版下载地址。若没有已核验的补丁构建或已独立验证的其他版本，停止生产放量。

## 3. 固定发布产物

本仓库基础 Compose 中的 `ghcr.io/mengmengcode/vocat:latest` 是上游默认值，不能证明包含本分支的候选更改。生产使用本次核验的固定产物，三服务必须完全一致；升级时同时替换 owner 和 proxy，单独重启主进程不会更新它们。

优先在已冻结、无未记录修改的源码快照自行构建。下列发布标识由操作者填写，不能使用 `latest`；保存源码清单以及未提交差异的私有归档。构建命令不会自动证明测试通过。

```bash
set -euo pipefail
umask 077
# 在本次已核验的 VoCat-sip 源码根目录执行：
git rev-parse HEAD
git status --short
read -r -p '本次固定发布标识（字母数字、点、横线）: ' VOCAT_RELEASE
[[ "$VOCAT_RELEASE" =~ ^[A-Za-z0-9][A-Za-z0-9.-]*$ ]] || exit 1
VOCAT_IMAGE="vocat-local:$VOCAT_RELEASE"
docker build --build-arg VERSION="$VOCAT_RELEASE" \
  --build-arg BUILD_TIME="$(git show -s --format=%cI HEAD)" \
  -t "$VOCAT_IMAGE" .
docker image inspect "$VOCAT_IMAGE" --format '{{.Id}} {{.Architecture}}'
docker run --rm --network none --entrypoint /opt/vocat/bin/vocat "$VOCAT_IMAGE" version
docker run --rm --read-only --network none --cap-drop ALL \
  --entrypoint /opt/vocat/bin/vocat "$VOCAT_IMAGE" media-check --require-amr --json
```

也可使用已实际发布且已核验的镜像 digest，例如由操作者提供完整的 `registry/repository@sha256:…`；先核实来源、架构、构建记录和实际内容，再显式拉取该 digest。本文不提供尚不存在的发行版本、镜像 digest 或二进制下载链接。本地标签不得覆盖重用；保留镜像 ID，可用 `docker image save` 导出供回滚。Dockerfile 的基础镜像和 apt 源仍可能变化，严格复现还需固定这些构建输入并保存依赖清单。

## 4. Docker 三服务全新安装（默认）

以下用于**空安装**。已有部署跳到升级章节，沿用原项目名、卷、路径和私有覆盖文件，不能照抄新项目以免建立空数据库。命令在同一个 Bash 会话执行；先完成上一节设置 `VOCAT_IMAGE`，当前目录仍是源码根目录。

```bash
set -euo pipefail
umask 077
sudo install -d -m 0700 /etc/vocat/compose /run/vocat
sudo install -m 0600 docker-compose.yml compose.data-owner.yaml /etc/vocat/compose/
printf 'COMPOSE_PROJECT_NAME=vocat\nVOCAT_IMAGE=%s\n' "$VOCAT_IMAGE" | \
  sudo tee /etc/vocat/compose/release.env >/dev/null
sudo chmod 0600 /etc/vocat/compose/release.env
sudo tee /etc/vocat/compose/compose.release.yaml >/dev/null <<'YAML'
services:
  vocat:
    image: ${VOCAT_IMAGE:?set a checked fixed image}
    pull_policy: never
    environment:
      VOCAT_ADDR: 127.0.0.1:7575
  vocat-qmi-proxy:
    image: ${VOCAT_IMAGE:?set a checked fixed image}
    pull_policy: never
  vocat-data-owner:
    image: ${VOCAT_IMAGE:?set a checked fixed image}
    pull_policy: never
YAML
sudo tee /etc/vocat/compose/control >/dev/null <<'BASH'
#!/bin/bash
set -euo pipefail
cd /etc/vocat/compose
exec docker compose --env-file release.env \
  -f docker-compose.yml -f compose.data-owner.yaml -f compose.release.yaml "$@"
BASH
sudo chmod 0700 /etc/vocat/compose/control
sudo /etc/vocat/compose/control config --quiet
sudo /etc/vocat/compose/control config --images
```

`control` 固定了项目与全部覆盖文件；后续所有管理命令均使用它。默认命名卷是项目 `vocat` 下的 `vocat-data`，通常为 `vocat_vocat-data`；以 `docker inspect` 的实际挂载为准。NAS 若不能保证一次性初始化与服务使用同一个命名卷，可在**首次初始化前**将两个服务的 `/opt/vocat/data` 都换为同一已核实的主机绝对路径。不得只改其中一个服务，不得将示例路径替换成另一已有实例的数据库。

初始化管理员仅执行一次；密码从终端输入，通过 stdin 进入 SQLite，不写环境文件或命令参数。使用 `--no-deps` 避免初始化时启动硬件服务：

```bash
set -euo pipefail
umask 077
read -rsp '首次管理员密码: ' VOCAT_BOOTSTRAP_PASSWORD; printf '\n'
printf '%s\n' "$VOCAT_BOOTSTRAP_PASSWORD" | \
  sudo /etc/vocat/compose/control run --rm --no-deps -T \
  --entrypoint /opt/vocat/bin/vocat vocat \
  bootstrap-admin --database /opt/vocat/data/vocat.db --username admin
unset VOCAT_BOOTSTRAP_PASSWORD
sudo /etc/vocat/compose/control up -d --no-build --pull never
sudo /etc/vocat/compose/control ps
sudo /etc/vocat/compose/control exec -T vocat-qmi-proxy /opt/vocat/bin/vocat qmi-proxy --check
curl --fail --show-error http://127.0.0.1:7575/healthz
curl --fail --show-error http://127.0.0.1:7575/readyz
```

三服务必须保留仓库的 host PID／network、设备、sysfs、共享锁挂载、120 秒优雅停止及 proxy healthcheck。`/sys` 只读之下的 `/sys/devices` 可写挂载供原所有者恢复 Raw-IP，不能因已有网卡连接成功而删掉它。容器使用主机网络，`ports:` 不隔离接口，实际开放范围由监听地址和主机防火墙决定。

如配置私有 SIM 准入文件，参照 [准入说明](AUTHORIZED_TEST_SIMS.md)，在主机仓库外使用 0700 目录与 0600 文件；向主进程和 owner 同时提供相同的只读挂载及 `VOCAT_AUTHORIZED_TEST_SIMS_FILE` 容器内绝对路径。不要把身份、号码、口令、Token 或真实主机信息写回 Compose 仓库。无此需求则不配置该变量。

## 5. 原生 systemd 三服务安装（可选）

需要完整的 systemd Linux 环境。先按前述依赖和 proxy 冲突检查安排设备维护；此路径不与 Docker 共用硬件。标准安装脚本当前不会自动启用可选 owner 布局，因此直接安装核验产物与本仓库三份 unit／drop-in。不要安装后再运行上游一键脚本覆盖它们。

先准备本机架构的 AMR 原生产物。沿用第 3 节的固定 `VOCAT_RELEASE` 标识；只选原生方案时不必先构建 Docker 镜像。源码构建示例：

```bash
set -euo pipefail
umask 077
# 在冻结的源码根目录；Go 1.25+、Node.js 20+、npm 已安装。
: "${VOCAT_RELEASE:?先设置本次固定发布标识}"
(cd web && npm ci && npm run build)
mkdir -p dist
VERSION="$VOCAT_RELEASE" BUILD_TIME="$(git show -s --format=%cI HEAD)" \
  sh scripts/build-linux.sh dist/vocat native
./dist/vocat version
./dist/vocat media-check --require-amr --json
sha256sum dist/vocat
```

也可从本次真实发行的附件取得匹配架构二进制和 `SHA256SUMS`，用 `sha256sum -c SHA256SUMS --ignore-missing` 并确认**所选二进制确实被校验**。静态 fallback 不等同于 native AMR 语音能力；不能把只支持 G.711 的检查视为运营商音频就绪。

```bash
set -euo pipefail
umask 077
# 以下为全新安装，不覆盖已有二进制、env 或数据库。
test ! -e /opt/vocat/bin/vocat || { echo '已有安装，请使用升级流程'; exit 1; }
sudo install -d -m 0755 /opt/vocat/bin
sudo install -d -m 0700 /opt/vocat/data /etc/vocat
sudo install -m 0755 dist/vocat /opt/vocat/bin/vocat
sudo tee /etc/vocat/env >/dev/null <<'ENV'
VOCAT_DATABASE_PATH=/opt/vocat/data/vocat.db
VOCAT_ADDR=127.0.0.1:7575
ENV
sudo chmod 0600 /etc/vocat/env
read -rsp '首次管理员密码: ' VOCAT_BOOTSTRAP_PASSWORD; printf '\n'
printf '%s\n' "$VOCAT_BOOTSTRAP_PASSWORD" | sudo /opt/vocat/bin/vocat \
  bootstrap-admin --database /opt/vocat/data/vocat.db --username admin
unset VOCAT_BOOTSTRAP_PASSWORD
sudo install -m 0644 deploy/vocat.service deploy/vocat-qmi-proxy.service \
  deploy/vocat-data-owner.service /etc/systemd/system/
sudo install -d -m 0755 /etc/systemd/system/vocat.service.d
sudo install -m 0644 deploy/vocat-data-owner.conf \
  /etc/systemd/system/vocat.service.d/data-owner.conf
sudo systemd-analyze verify /etc/systemd/system/vocat.service \
  /etc/systemd/system/vocat-data-owner.service /etc/systemd/system/vocat-qmi-proxy.service
sudo systemctl daemon-reload
sudo systemctl enable vocat-qmi-proxy vocat-data-owner vocat
sudo systemctl start vocat-qmi-proxy
sudo /opt/vocat/bin/vocat qmi-proxy --check
sudo systemctl start vocat-data-owner
sudo systemctl start vocat
sudo systemctl status vocat-qmi-proxy vocat-data-owner vocat --no-pager
curl --fail --show-error http://127.0.0.1:7575/readyz
```

两个客户端使用相同二进制、字面绝对数据库路径、网络／PID 命名空间、私有 socket 和共享 `/run/vocat`、`/run/lock`。保留 unit 的权限例外、`RuntimeDirectoryPreserve=yes` 和 120 秒停止预算。当前候选通过 `Wants` 保留 proxy 故障时的原 owner，并用 `ExecStartPre` 检查 proxy unit／socket；升级前检查实际部署的 `systemctl cat`，不要仅凭文件名认为已应用新关系。此监督关系的真实硬件故障验收状态见第 1 节。

仅前台诊断使用显式 `vocat serve`；root TTY 下无参数运行可能进入菜单，不应作为手动启动服务器的命令。

## 6. UI、鉴权与接口开放

默认示例仅监听 `127.0.0.1:7575`。可通过受信任 VPN、HTTPS 反向代理或 SSH 本地转发访问管理 UI；如需 LAN 直连，将 `VOCAT_ADDR` 改为已核实的本机私网地址并配置主机防火墙。HTTPS 部署设置 `VOCAT_SECURE_COOKIES=true`，代理需要支持 WebSocket。保存配置后按所选部署正常重启主进程。

```bash
set -euo pipefail
umask 077
# 在管理员客户端另一个 Bash 终端建立转发：
read -r -p 'SSH 目标（账号@服务器）: ' VOCAT_SSH_TARGET
: "${VOCAT_SSH_TARGET:?必须填写 SSH 目标}"
ssh -N -L 7575:127.0.0.1:7575 "$VOCAT_SSH_TARGET"
# 随后访问 http://127.0.0.1:7575
```

| 接口 | 开放及鉴权 |
| --- | --- |
| 管理 UI／`/api/*` | 使用 SQLite 中的管理员账号与会话；变更接口按服务要求携带 CSRF。登录路径 `/api/auth/login`，会话检查 `/api/auth/session`。不在脚本明文写管理员口令。 |
| `/healthz`、`/readyz`、`/metrics` | 进程／数据库探针；不证明 SIM 注册、承载、DNS、公网流量或媒体工作。不需为健康检查创建管理员账号。 |
| 短信只读 API | 默认关闭；独立监听默认 `127.0.0.1:8787`，使用独立 Bearer Token。LAN／VPN 须显式启用，规则见 [短信 API](SMS_API.md)。Token 可读取全部线路收件箱；`slotId` 是筛选而非权限。 |
| SIP 分机／固定 peer | 默认关闭；设置精确 UDP 监听 IP:port、独立长密码、CIDR 和逐线／方向权限，避开已有 PBX 端口。当前为可信 LAN／VPN UDP 服务，无 TLS／SRTP；参阅 [SIP 网关](SIP_GATEWAY.md)。媒体地址／端口以实际 SDP 和网络规划为准，不盲开整段 UDP。 |
| HTTP／SOCKS5 数据导出 | 在代理设置中单独启用鉴权及可信来源；仅当原 SIM 的普通数据承载被确认持有才可供网。监听成功不等于可供网，不通过宿主默认路由伪造蜂窝成功。 |

## 7. 逐 SIM 配置与纯 Host IMS 启用

先在已鉴权的设备页面核实当前物理设备、实时 SIM、USB 代次及接口。发现端口不等同于已配置线路。首次迁移旧配置时，必须在当前已识别 SIM 上显式应用；不能把无 ICCID 历史的旧设备配置默认归给新卡。

1. 为每张 SIM 单独设置蜂窝方式、蜂窝自动连接、Wi-Fi 自动连接、默认业务路由及 Wi-Fi 射频策略；保存后核实当前卡的投影。默认路由只影响新业务，已建立通话仍归原接入。参考 [独立接入](INDEPENDENT_ACCESS.md)。
2. **QDC507 纯 Host IMS**：显式选择蜂窝方式 `host_ims` 和默认路由 `host_ims`；按业务开启蜂窝连接，普通数据开关与 IMS 承载分开配置。不要用 AT 拨号、模块提交短信或旧 ADB 音频证明此路径成功。
3. `host_ims_profile_id=0` 表示 modem 默认；明确指定时使用运营商已经存在且启用 P-CSCF 发现的 3GPP profile（1–255）。VoCat 不创建、编辑或删除 profile；APN 字符串本身不保证有 P-CSCF。IP 家族需同时满足 profile 和实际运行时。
4. 部分已测固件需要把原生基带 IMS 交给主机。只能在空闲维护窗口、记录原设置和设备／SIM 身份后执行该固件已验证的交接；[专门说明](HOST_IMS_DEPLOYMENT.md#baseband-ims-handoff)记载了测试 QDC507 的 `AT+QCFG="ims",2` 加完整重启。它不是通用安装步骤，程序也不会自动执行。维护结束按原值恢复并重启，不能对任意 modem 盲用。
5. VoWiFi 按该 SIM 配置 ePDG／运营商偏好。`exclusive` 持有射频关闭／恢复检查点，`coexist` 不接管蜂窝射频或数据；改变策略前先正常停止并确认清理。同 SIM 双注册需要固件与运营商实测。此次范围只要求成功连接。
6. SIP、任务、自动接听、短信桥和通知分别按需授权，首次验收前保持未使用功能关闭；启用业务自动化之前确认目标线路与接入，避免意外拨号／短信。不同 SIM 的失败不得用另一张卡自动顶替。

传统 AT modem 若与 ModemManager 争串口，应只对已核实的那台设备安排 inhibitor 生命周期，保留另一条 Host IMS 所需的 ModemManager；停止并清理 VoCat 后再释放原 inhibitor。具体做法见 [Host IMS 部署说明](HOST_IMS_DEPLOYMENT.md)。

## 8. 普通数据可选物理恢复

默认不开启。仅当确实需要、接受完整模块重启带来的临时中断，并且该 SIM 为明确的 **data-only** 策略时启用。Host IMS、Wi-Fi、未知／冲突策略、RF-off、存在语音呼叫或不能确认原卡时均不准许重启。

使用已鉴权的 `/api/devices/discovered` 中的 `discovery_key`，结合 `at_ports` 与主 `at_port`，选择同一物理 modem 的**另一个已验证 AT 端口**。不是应用线路 ID。优先 `/dev/serial/by-id/…` 稳定别名；USB 重新枚举会使 `ttyUSB` 数字漂移。不要猜测端口或使用通配符。

在私有配置中只给 **owner** 增加：

```text
VOCAT_DATA_OWNER_RECOVERY_PORTS={"<实际 discovery_key>":"/dev/serial/by-id/<已验证次 AT 别名>"}
```

上述占位符不能原样启用。原生写入 `/etc/vocat/env` 时须按 systemd EnvironmentFile 的引号规则保留 JSON，例如将整个值置于单引号内；Compose 在私有覆盖文件 owner 的 `environment` 中使用带引号的 JSON 字符串，并先运行 `control config --quiet`。这是恢复配置变更，应先正常禁用普通数据、确认释放后停主进程与 owner，再启动 owner／主进程。保留相同数据库、共享锁与 `/sys/devices` 可写权限。

恢复只在确认永久传输损失及原设备／SIM／策略／RF／呼叫状态后，持久化一次重启意图，随后提交最多一次 `AT+CFUN=1,1`。收到 OK 不代表清理完成。必须观察原 USB 代次消失、原接口／路由／规则／DNS 精确清理，才允许新的数据会话。

活动 owner 死亡恢复还要求同一 boot／namespace 内的 v2 原进程身份和已提交 host receipt。旧记录、未确认 Start、配置 pending、重启意图已消耗／结果不确定、主机断电、换卡或未知主机对象仍保留 fence。不能手工删除 fence、重用旧 CID／PDH、借用外来承载、广泛清表或反复重启来强行连接。完整约束见 [DATA_OWNER_RECOVERY](DATA_OWNER_RECOVERY.md)。

## 9. 验收与日常观察

按实际启用功能验收，结果附本次产物 hash 与时间，敏感原始证据留在私有运维存储。不要在仓库加入号码、SIM 身份、口令、真实主机地址或完整原始日志。

1. 三服务存活；proxy `--check` 成功；`/healthz`、`/readyz` 连续成功。检查实际镜像／二进制是否一致，数据库／socket／锁挂载是否一致。
2. 使用首次管理员登录。普通重启后原管理员和已有会话仍有效；不要在重装时初始化新数据库或重置密码来掩盖卷选错。
3. 核实每张卡实时身份、原射频／原生 IMS／profile 设置，确认未启用的业务保持关闭。
4. 普通数据分别验收所需 IPv4、IPv6、双栈：新鲜、绑定原 modem 接口的公网查询与实际 HTTPS 流量成功；双栈观察两类地址／路由，不能仅凭 UI“连接”或历史缓存。IPv6 DNS 要做实际查询。
5. Host IMS 观察其真实承载、P-CSCF、注册、短信就绪；在获得具体测试授权的号码上验收双向通话／短信、双向音频、挂断清理及必要 SIP 权限。已有报告可作参考，不替代新环境验收。VoWiFi 本轮只验收连接；浏览器物理音频继续标为延期。
6. 故障注入仅在另行授权的隔离维护窗口进行，记录精确原进程、启动标识、退出、恢复和清理证据；不得把生产 `kill -9` 当常规上线步骤。本手册不会自动拨号、发短信或注入故障。
7. 验收后正常停止测试接入，核实原主机网络、SIM、profile 与原生 IMS 状态及其他应用均恢复。

```bash
set -euo pipefail
umask 077
# Docker：
sudo /etc/vocat/compose/control ps
sudo /etc/vocat/compose/control logs --tail 200 vocat vocat-data-owner vocat-qmi-proxy
docker inspect vocat vocat-data-owner vocat-qmi-proxy \
  --format '{{.Name}} image={{.Image}} restarts={{.RestartCount}} pid={{.State.Pid}}'
# systemd：
sudo systemctl cat vocat vocat-data-owner vocat-qmi-proxy
sudo journalctl -u vocat -u vocat-data-owner -u vocat-qmi-proxy --since '-15 min' --no-pager
```

进程探针不是业务 SLA。持续关注数据库可读写、磁盘容量、proxy 重启、未决清理、实际 SIM 注册、IPv6 DNS 和实际出口查询；录音默认位于数据库旁的 `recordings`，须纳入容量和备份管理。日志可能包含敏感业务信息，只在私有通道保留与脱敏分享。

## 10. 备份、升级与回滚

### 共同顺序与保留项

先安排维护，停止自动任务及新业务，正常结束电话、关闭逐卡 IMS／Wi-Fi／普通数据，等待每个原所有者确认清理。然后按**主进程 → owner → proxy**停机；确认全部退出后备份。若清理未确认，保留正在清理的原 owner／proxy，停止升级并调查，不能为了替换程序把能力杀掉。

必须备份：完整数据目录（数据库及可能存在的 `-wal`／`-shm`、录音、私有持久状态）、配置与私有准入文件、Compose 项目／卷／绑定路径、全部 unit／drop-in、原镜像或二进制及其 hash、原 ModemManager 配置／临时补丁选择。备份目录用 0700、文件用 0600，限制读取者。

**不删除、不重建、不改名 `/run/vocat`、`/run/lock` 或其锁文件。** 保留原 inode 和命名空间。运行时目录是正在运行系统的所有权边界，不能从另一台主机／另一个 boot 的备份覆盖恢复，也不能把“没有进程”当作 fence 已可删除。

数据库迁移前保存整套停机备份；仅复制运行中的 `vocat.db` 不能保证一致性。全目录备份须在进程已经正常停止后完成。恢复旧程序时不能盲目打开已迁移的新数据库；必要时成套恢复旧产物与其停机快照，且先证明当前所有接入和主机资源已正常释放。

### Docker 升级命令

已有部署先记录实际项目和挂载，沿用原配置；下面的 `control` 路径仅适用于按本文建立的部署。不得使用 `down -v`、卷清理或创建新空卷来做升级。

```bash
set -euo pipefail
umask 077
docker inspect vocat --format '{{index .Config.Labels "com.docker.compose.project"}}'
docker inspect vocat --format '{{json .Mounts}}'
# 先在 UI 正常停业务、禁用接入并确认清理，再依次停：
sudo /etc/vocat/compose/control stop vocat
sudo /etc/vocat/compose/control stop vocat-data-owner
sudo /etc/vocat/compose/control stop vocat-qmi-proxy
VOCAT_BACKUP="/var/backups/vocat/$(date -u +%Y%m%dT%H%M%SZ)"
sudo install -d -m 0700 "$VOCAT_BACKUP"
sudo cp -a /etc/vocat/compose "$VOCAT_BACKUP/compose"
# 一次性容器不启动依赖、不访问设备 API；完整备份同一数据卷：
sudo /etc/vocat/compose/control run --rm --no-deps -T \
  --entrypoint /bin/sh -v "$VOCAT_BACKUP:/backup" vocat \
  -ec 'umask 077; tar -C /opt/vocat/data -cpf /backup/data.tar .'
sudo chmod 0600 "$VOCAT_BACKUP/data.tar"
```

在业务停机前可预先构建／拉取新固定产物并运行无硬件 `version`、`media-check`。备份完成后，将 `release.env` 的 `VOCAT_IMAGE` 改为本次已核验的新固定值；如 unit／Compose 有更新，保留原私有配置并审查合并，不盲覆盖。再次检查三服务镜像配置，然后统一重建三服务：

```bash
set -euo pipefail
umask 077
sudo /etc/vocat/compose/control config --quiet
sudo /etc/vocat/compose/control config --images
sudo /etc/vocat/compose/control up -d --no-build --pull never
sudo /etc/vocat/compose/control ps
curl --fail --show-error http://127.0.0.1:7575/readyz
```

重新登录／验证已有会话、配置和各项已启用功能，通过后再恢复逐卡自动连接及任务。不要通过容器内自更新替换二进制；容器应靠固定镜像重建。

### 原生升级命令

先完成共同停业务与清理步骤，然后：

```bash
set -euo pipefail
umask 077
sudo systemctl stop vocat
sudo systemctl stop vocat-data-owner
sudo systemctl stop vocat-qmi-proxy
VOCAT_BACKUP="/var/backups/vocat/$(date -u +%Y%m%dT%H%M%SZ)"
sudo install -d -m 0700 "$VOCAT_BACKUP"
sudo tar -C /opt/vocat/data -cpf "$VOCAT_BACKUP/data.tar" .
sudo cp -a /opt/vocat/bin/vocat "$VOCAT_BACKUP/vocat"
sudo cp -a /etc/vocat "$VOCAT_BACKUP/etc-vocat"
sudo cp -a /etc/systemd/system/vocat.service /etc/systemd/system/vocat-data-owner.service \
  /etc/systemd/system/vocat-qmi-proxy.service /etc/systemd/system/vocat.service.d "$VOCAT_BACKUP/"
# 在当前已核验源码目录，用新产物替换；不要改变数据库和共享锁：
sudo install -m 0755 dist/vocat /opt/vocat/bin/vocat.new
sudo mv /opt/vocat/bin/vocat.new /opt/vocat/bin/vocat
# 如有 unit 更新，此时按审查结果安装并执行 daemon-reload：
sudo systemctl daemon-reload
sudo systemctl start vocat-qmi-proxy
sudo /opt/vocat/bin/vocat qmi-proxy --check
sudo systemctl start vocat-data-owner
sudo systemctl start vocat
curl --fail --show-error http://127.0.0.1:7575/readyz
```

### 回滚执行规则

1. 新版本失败时先禁止新业务，按共同顺序正常释放并停止。保留失败版本日志、完整数据库／状态及配置副本，勿用旧数据库掩盖未清理承载。清理不确定时停止回滚和物理重启，按所有权排障处理。
2. 若确认数据库与旧版本兼容，Docker 将 `VOCAT_IMAGE` 恢复为备份中的原固定值并统一重建三服务；原生从备份恢复二进制和原 unit／drop-in，执行 `daemon-reload`，按 proxy → owner → main 启动。恢复原环境文件，保留原管理员、会话、数据和共享锁。
3. 若迁移不兼容，在确认新版本全部资源释放后恢复**匹配旧版本的整套停机数据快照**。原生将当前 `/opt/vocat/data` 保留在带时间戳的隔离目录，在同一路径新建 0700 空目录后解包；不要把旧快照覆盖进含新 `-wal` 的目录。Docker 将快照解包到**新的空命名卷**，审查覆盖文件，把主进程与 owner 的数据挂载同时指向该恢复卷；原卷保留供调查，不能一边恢复一边运行。确认 `docker inspect` 两者挂载一致再启动。
4. 数据恢复会回到快照时间，之后的短信、配置和记录可能不在快照内；已发送的运营商业务不能撤回，也不能重新执行来“补齐”。保持自动任务关闭直至完成核对。
5. 回滚 ModemManager 须另行维护：先清理 VoCat 和其他原客户端，只移除本次拥有的临时 drop-in／二进制选择，恢复原发行版服务；不能删除其他人的配置。发行版此前 SIGSEGV 仍须保留为风险，恢复后短时运行不能证明问题消失。

仅当已停机、资源已确认释放，且确认数据库兼容旧程序时，产物回滚命令示例：

```bash
set -euo pipefail
umask 077
# Docker：VOCAT_BACKUP 指向选定的本次停机备份；保留原失败配置副本后执行。
sudo install -m 0600 "$VOCAT_BACKUP/compose/release.env" /etc/vocat/compose/release.env
sudo /etc/vocat/compose/control config --quiet
sudo /etc/vocat/compose/control up -d --no-build --pull never
# 原生：与 Docker 分别择一；先按备份恢复实际变更过的 unit/drop-in/env。
sudo install -m 0755 "$VOCAT_BACKUP/vocat" /opt/vocat/bin/vocat.new
sudo mv /opt/vocat/bin/vocat.new /opt/vocat/bin/vocat
sudo systemctl daemon-reload
sudo systemctl start vocat-qmi-proxy
sudo /opt/vocat/bin/vocat qmi-proxy --check
sudo systemctl start vocat-data-owner
sudo systemctl start vocat
```

若必须恢复数据库，默认 Docker 命名卷方案可先建立空恢复卷，保留原卷不变。以下仍要求所有运行实例已停止、旧资源已正常释放；`VOCAT_ROLLBACK_IMAGE` 必须是本次备份对应、已在本机核验的旧镜像，恢复后新产生的数据不在备份内：

```bash
set -euo pipefail
umask 077
read -r -p '本机已核验的旧镜像固定引用: ' VOCAT_ROLLBACK_IMAGE
docker image inspect "$VOCAT_ROLLBACK_IMAGE" --format '{{.Id}}'
VOCAT_RESTORE_VOLUME="vocat-restore-$(date -u +%Y%m%dT%H%M%SZ)"
docker volume create "$VOCAT_RESTORE_VOLUME"
docker run --rm --network none --entrypoint /bin/sh \
  --mount "type=volume,src=$VOCAT_RESTORE_VOLUME,dst=/restore" \
  --mount "type=bind,src=$VOCAT_BACKUP,dst=/backup,readonly" \
  "$VOCAT_ROLLBACK_IMAGE" \
  -ec 'test -z "$(ls -A /restore)"; umask 077; tar -C /restore -xpf /backup/data.tar'
```

随后恢复备份对应的 Compose／镜像配置，并在私有覆盖文件顶层设置 `volumes.vocat-data.external: true` 和 `volumes.vocat-data.name: <上一步实际卷名>`；两个客户端仍使用同一 `vocat-data` 源。`control config --quiet` 检查通过后启动，再用 `docker inspect` 确认两者都挂载这个恢复卷。此步骤不适用于原先使用 bind 的部署；bind 恢复应在已确认的原数据路径旁建立空目录并保留失败目录，不覆盖未知路径。

## 11. 常见故障处置

| 现象 | 检查与安全处置 |
| --- | --- |
| 管理员密码错误／升级像全新安装 | 核对 bootstrap 与运行实例的实际数据库绝对路径、项目名、命名卷和 NAS bind。不要重复初始化、重置已有管理员或换空卷掩盖路径错误。 |
| proxy 启动失败或 unhealthy | 查原 proxy 归属、QMI 工具路径、独立 unit 状态／socket、容器 host namespace。已有服务的 proxy 不可强行接管；安排全客户端正常维护交接。 |
| owner 无法启动／连接不上 Unix socket | 检查完全相同的产物、DB、`VOCAT_DATA_OWNER_SOCKET`、0700 `/run/vocat`、namespace 与 0600 socket；检查恢复 JSON 是否格式错误、键或端口重复。不要 chmod 为全员可写。 |
| Raw-IP permission denied／重枚举后连接失败 | 确认 Docker 的 `/sys/devices` 可写 overlay 与原生 `ReadWritePaths`，并核实原 USB 和接口代次。不要对替换接口广泛 flush／down。 |
| 数据显示连接但公网失败 | 做新鲜的原接口绑定 DNS／HTTPS 查询，分别核查家族、地址、规则和 DNS；UI 状态与 `/readyz` 不构成连通性证据。IPv6 可能需要已批准的完整模块维护，不能称为自动修复。 |
| cleanup pending／fence 拒绝新操作 | 保留原 owner、proxy、DB 和锁；先正常 Disable／Stop 重试原清理，核对原 USB／SIM、原 namespace 及精确主机对象。未知 Start、已耗尽 reset 或旧 boot 需运维调查。禁止删除日志记录、重用旧 CID／PDH、强制 claim takeover 或清空所有路由。 |
| ModemManager SIGSEGV | 记录软件版本、时间、受影响接入与私有 crash 信息，按匹配补丁的核验流程处理。不要反复重启管理器后继续宣称通过；其重启可能破坏原 Host IMS 所有权证明。 |
| Host IMS 无 P-CSCF／注册失败 | 核对当前 SIM、运营商既有 IMS profile、P-CSCF 发现、IP 家族、原生 IMS 交接及 MM 实际 bearer。不要创建／修改未知 profile，不回退 AT 或别的 SIM。 |
| 语音单向／无声 | 检查 native AMR `media-check`、原 SDP／RTP 路由和防火墙、SIP codec、实际音频端点权限。录音或 API 有 PCM 不等于物理浏览器音频通过。 |
| 临时 ModemManager systemd 203 | `ProtectHome` 会隐藏 `/root` 下构建；把已核验临时可执行文件放在 `/usr/local/libexec` 等允许路径，保留服务加固和发行版二进制。 |
| 关闭超时／服务无法正常停 | 保留 120 秒监督预算并检查原清理 worker／传输；不要删锁、缩短 grace 或把强杀视为成功退出。不能确认清理时暂停升级。 |

本手册描述的是可执行部署路径及已知限制。面向生产放量前，必须明确选择并核验 ModemManager 依赖、固定实际候选产物，并按目标环境确认 IPv6 冷启动；选择原生方案时还须核实当前监督关系对应的真实硬件故障记录。断电／整机重启和旧版未知所有权记录不能从已通过的 Compose 进程故障案例直接推断。
