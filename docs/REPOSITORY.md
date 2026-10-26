# 仓库与提交约定

VoCat-sip 是独立 Go／React 仓库，远端为 `unikgyd/VoCat-sip`，现有默认分支是 `master`。
“提交到主分支”沿用该分支，不额外改名为 `main`。Windows 的 `unikgyd/VoWin` 仓库使用 `main`。
两者在本机可以嵌套存放，但不是隐式 Git 子模块；分别管理提交记录。

## 纳入源码提交

- Go 后端、React／TypeScript 前端、测试源码与公开合成样例。
- `go.mod`／`go.sum`、`web/package.json`／`web/package-lock.json`。
- 文档、构建脚本、CI 配置和依赖许可。

## 保留本地

- `build/`、`publish/`、`web/dist/`、`web/node_modules/`、`web/.vite/`。
- 运行数据库、日志、Cookie／Session、环境变量文件、私钥与证书包。
- 本机诊断、真实 SIM 身份、运营商抓包、部署凭据和代理配置。

构建产物由发布流程生成；Go 的嵌入式前端需要先构建 `web/dist`，不能把未提交的本机
构建产物当成新源码已经验证的证据。普通文档放在 `docs/`；发布目录中的历史验收归档
是本机工作记录，不作为其他克隆必然可见的链接依赖。

## 当前提交状态

2026-10-26 的独立接入应用层是开发进度保存，仍有实现与验收事项，见
[能力并集清单](FEATURE_UNION.md) 和[独立接入说明](INDEPENDENT_ACCESS.md)。
遵照本轮要求，先完成实现，最后统一测试与构建；本次文档和 Git 整理不执行测试、构建
或部署。已有历史通过记录不适用于新增的整合代码，当前提交不能作为发布验收。

`.gitattributes` 固定脚本及 Go／前端源码的 LF，避免 Windows 检出破坏 Linux 脚本。
忽略规则只控制未跟踪文件，不删除本地资料或覆盖其他未提交工作。
