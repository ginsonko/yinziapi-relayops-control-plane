# RelayOps Control Plane

[![CI](https://github.com/ginsonko/yinziapi-relayops-control-plane/actions/workflows/ci.yml/badge.svg)](https://github.com/ginsonko/yinziapi-relayops-control-plane/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

RelayOps 是面向 NewAPI、Sub2API 及其兼容分支的自托管上游采购与定价控制台。它运行在 API 请求链路之外，以低频、可审计的方式采集模型、分组、价格、余额和消耗信息，并通过价格映射、分组镜像与受控写入减少人工维护。

> 当前仓库是首个公开 beta。请先使用脱敏夹具和隔离目标验证能力，再开启后台采集、远程写入或无人值守。连接成功不代表计费语义已经验证。

## 核心能力

- 原生读取 NewAPI 和 Sub2API 上游目录、价格及可用的余额/用量合同；
- 使用 Connector Manifest 兼容路径、Header 或登录字段不同的 NewAPI/Sub2API 分支；
- 使用声明式 JSON Connector 读取其他稳定 JSON 上游；
- 维护价格映射、分组镜像、例外模型、映射草案和一键上架计划；
- 同步上游模型的上架/下架、分组逐模型基础价格和计费单位；
- 通过 `preview -> revision/CAS -> apply -> readback -> conditional rollback` 执行写入；
- 使用最后可信快照承受网络、TLS、DNS、429 和上游 5xx 抖动；
- 提供活动、重点关注、经营数据、容量保留和高信息密度副屏；
- 将凭据加密保存在独立数据目录，日志和配置导出不保存凭据正文；
- 与 NewAPI 数据面隔离，RelayOps 停止或回滚不会中断用户 API 请求。

## 兼容范围

| 场景 | 当前能力 | 推荐方式 |
|---|---|---|
| NewAPI 作为上游 | 原生目录、价格、余额和用量采集 | `newapi` Source Connector |
| Sub2API 作为上游 | 原生客户、管理员和上游模式采集 | `sub2api` Source Connector |
| 魔改 NewAPI/Sub2API | 路径、Header、登录字段可配置 | Connector Manifest |
| 任意稳定 JSON 上游 | 有界 GET/POST、分页和字段映射 | JSON Connector |
| NewAPI 作为本站控制面 | 九类价格 Option、渠道和可选分组模型合同 | NewAPI Target Adapter |
| Sub2API 作为本站控制面 | **尚未内置** | 实现独立 Target Adapter |

Source Connector 只负责读取上游；Target Adapter 才能修改本站控制面。两者不能互换。标准 NewAPI 如果没有 `/api/option/group-model-pricing` 扩展，RelayOps 会将分组逐模型定价标记为不可用，不会猜测字段或模拟成功。

## 分组逐模型定价

RelayOps 支持 NewAPI 的全局 `ModelRatio`、`CompletionRatio`、`ModelPrice` 等 Option，也定义了可选的分组逐模型原子合同。二者与最终利润倍率的职责不同：

- `ModelRatio` / `CompletionRatio` 或 `ModelPrice` 表示模型基础价格；
- 分组逐模型合同允许同一模型在不同本站分组使用不同基础价格；
- `GroupRatio` 是最终用户分组倍率，仍在基础价格之后乘算；
- `per_request`、`per_second` 和 `fixed_duration` 必须保留真实计费单位，不能按模型名猜测。

例如输入/输出绝对基础价为 `$5/$30` 时，NewAPI 的 token 比率合同是 `ModelRatio=2.5`、`CompletionRatio=6`：输入为 `2 * 2.5 = 5`，输出为 `2 * 2.5 * 6 = 30`。详见[分组模型定价合同](docs/GROUP_MODEL_PRICING.md)。

## 五分钟启动

要求 Docker Engine 24+ 与 Docker Compose v2。

```bash
export RELAYOPS_ADMIN_TOKEN="$(openssl rand -hex 32)"
docker compose up -d --build --wait
```

默认地址为 `http://127.0.0.1:18180`。公开 Compose 没有固定管理密钥，并且默认关闭远程写入、无人值守和后台 worker。管理令牌必须显式提供且不少于 32 个字符。

第一次启动建议按以下顺序操作：

1. 登录控制台并添加加密凭据；
2. 添加一个上游，手动执行只读采集；
3. 添加本站 NewAPI，核对能力矩阵；
4. 生成预览，核对模型数、分组、币种、倍率与计费单位；
5. 完成隔离读回测试后，再按需开启后台 worker；
6. 只有目标能力和回滚路径均通过验证后，才开启远程写入及无人值守。

```bash
export RELAYOPS_BACKGROUND_WORKERS_ENABLED=true
export RELAYOPS_REMOTE_WRITES_ENABLED=true
export RELAYOPS_AUTOMATION_ENABLED=true
docker compose up -d
```

可以复制 [`.env.example`](.env.example) 作为配置起点，但必须生成自己的管理令牌。不要将管理令牌、`.env`、SQLite 数据库或 `master.key` 提交到版本库。远程访问应使用经过认证的 HTTPS、VPN 或 SSH 隧道，不要直接公开管理端口。

## 本地隔离验收

仓库提供只监听回环地址的 NewAPI 夹具，不连接真实 NewAPI，也不会产生模型费用：

```powershell
go run ./cmd/relayops-newapi-fixture -listen 127.0.0.1:18192
```

另开终端启动 RelayOps：

```powershell
$env:RELAYOPS_LISTEN = "127.0.0.1:18191"
$env:RELAYOPS_DATA_DIR = "$PWD\tmp\local-acceptance"
$env:RELAYOPS_STATIC_DIR = "$PWD\web\dist"
$env:RELAYOPS_ADMIN_TOKEN = "replace-with-a-random-token-at-least-32-characters"
$env:RELAYOPS_DEV_MODE = "true"
$env:RELAYOPS_ALLOW_LOOPBACK_TARGETS = "true"
$env:RELAYOPS_REMOTE_WRITES_ENABLED = "false"
$env:RELAYOPS_AUTOMATION_ENABLED = "false"
$env:RELAYOPS_BACKGROUND_WORKERS_ENABLED = "false"
go run ./cmd/relayops
```

夹具管理 Token 为 `fixture-management-token`，用户 ID 为 `17`；也可以使用夹具账号 `relayops-root` / `relayops-local-only`。这些值只对本地回环夹具有效。

## 开发与测试

后端需要 Go 1.23+：

```bash
go test ./... -count=1
go vet ./...
go build ./cmd/relayops
```

前端需要 Node.js 20+ 与 Corepack：

```bash
cd web
corepack enable
pnpm install --frozen-lockfile
pnpm build
```

登录配对助手合同测试：

```bash
node --test tests/browser-extension-contract.test.mjs
```

## 二次开发路线

当魔改站点无法被原生连接器读取时，按以下顺序扩展：

1. 仅路径、Header 或登录字段变化：Connector Manifest；
2. 有稳定 JSON，但不再兼容原协议：JSON Connector；
3. 鉴权、分页、单位或目录语义无法声明式表达：原生 Go Source Connector；
4. 需要写入非 NewAPI 本站：另行实现 Target Adapter。

使用 Codex 等工具时，只提供脱敏 fixture、相对路径、字段结构和合同测试；不要提供真实 Cookie、Token、密码、HAR 正文、数据库或生产写权限。完整流程见[连接器与目标适配器开发指南](docs/CONNECTOR_DEVELOPMENT.md)。

## 文档

- [架构设计](docs/ARCHITECTURE.md)
- [HTTP API 参考](docs/API_REFERENCE.md)
- [分组模型定价合同](docs/GROUP_MODEL_PRICING.md)
- [NewAPI 目标集成合同](docs/NEWAPI_TARGET_INTEGRATION.md)
- [连接器与目标适配器开发](docs/CONNECTOR_DEVELOPMENT.md)
- [可执行脱敏示例](examples/)
- [贡献指南](CONTRIBUTING.md)
- [安全策略](SECURITY.md)
- [公开发布清单](docs/PUBLIC_RELEASE_CHECKLIST.md)
- [变更日志](CHANGELOG.md)

## 项目边界

RelayOps 不是 API 网关，不代理用户模型请求，也不参与请求级路由、预扣费或退款。按秒/固定时长的真实扣费由目标 NewAPI 数据面实现；RelayOps 负责保存、同步和验证控制面合同。未知计费语义、缺少目标能力或无法读回时，系统应停在预览并保留旧可信状态。

本项目与 New API、Sub2API 及相关上游服务的维护者无隶属关系。产品名和商标归各自权利人所有。

## License

Apache License 2.0。第三方依赖及其许可证见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
