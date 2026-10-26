# 仓库与提交约定

VoCat-sip 是独立 Go／React 仓库，远端为 `unikgyd/VoCat-sip`。按用户最新要求，
本地开发与后续提交统一放在 `main`，现有 `master` 历史保留。
远端默认分支目前仍是 `master`；创建本地 `main` 不会自动推送或更改远端默认分支。
Windows 的 `unikgyd/VoWin` 也使用 `main`。两者在本机可以嵌套存放，但不是隐式
Git 子模块；分别管理提交记录。

## 纳入源码提交

- Go 后端、React／TypeScript 前端、测试源码与公开合成样例。
- `go.mod`／`go.sum`、`web/package.json`／`web/package-lock.json`。
- 文档、构建脚本、CI 配置和依赖许可。

## 保留本地

- `build/`、`publish/`、`web/dist/`、`web/node_modules/`、`web/.vite/`。
- 运行数据库、日志、Cookie／Session、环境变量文件、私钥与证书包。
- 本机诊断、真实 SIM 身份、运营商抓包、部署凭据和代理配置。
- 真实通话录音及其元数据保存在数据库旁的 `recordings/`；自定义目录也只作为运行资料。

构建产物由发布流程生成；Go 的嵌入式前端需要先构建 `web/dist`，不能把未提交的本机
构建产物当成新源码已经验证的证据。普通文档放在 `docs/`；发布目录中的历史验收归档
是本机工作记录，不作为其他克隆必然可见的链接依赖。

## 当前提交状态

2026-10-26 的 Linux 实现已按要求先实现，再统一测试与构建；本轮结果见
[软件验收记录](VERIFICATION_20261026.md)、[能力清单](FEATURE_UNION.md)和
[独立接入说明](INDEPENDENT_ACCESS.md)。记录绑定实际源码快照，历史结果不代替新源码。
当前为开发候选，真实硬件／运营商与部署验收单列；文档及 Git 整理不替换现用服务。

2026-10-27 已补充 [NAS 实机验收](VERIFICATION_20261027_LIVE.md)：双线路注册、
电信实拨振铃、原资源清理和环境回滚。受控 SIM 准入文件只保留在私有运行目录；
源码根目录的 `authorized-test-sims*.json` 也列入忽略规则。

`.gitattributes` 固定脚本及 Go／前端源码的 LF，避免 Windows 检出破坏 Linux 脚本。
忽略规则只控制未跟踪文件，不删除本地资料或覆盖其他未提交工作。
