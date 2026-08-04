# RelayOps HTTP API 参考

本文对应 `relayops-v3-r49-4-settings-busy-retry-20260804`。正式公开版本应从路由测试生成并校验此清单，避免文档漂移。

## 1. 基础约定

- 管理 API 前缀：`/api/v1`
- 管理认证：`X-RelayOps-Token: <admin-token>`
- 前端合同：`X-RelayOps-API-Contract: <contract-version>`
- JSON 请求：`Content-Type: application/json`
- 成功：`{"success":true,"data":...}`
- 失败：`{"success":false,"message":"..."}`

客户端可以不发送合同 Header；一旦发送且与服务端不一致，服务端返回 `409 Conflict`，客户端应重新加载静态资源后重试。

## 2. 副作用分类

| 标记 | 含义 |
|---|---|
| `RO` | 仅本地读取 |
| `LR` | 本地数据库写入，不访问远程写接口 |
| `RR` | 读取上游或目标站点，不应产生模型调用费用 |
| `RW` | 修改上游/目标配置，受远程写开关约束 |
| `BILL` | 真实模型测试，可能产生费用，必须显式操作 |

## 3. 无管理令牌端点

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/health`, `/api/v1/health` | 服务状态与 API 合同 |
| OPTIONS/POST | `/api/v1/pair/{token}` | 浏览器助手跨源预检/提交登录态 |
| GET | `/api/v1/pair/{token}/status` | 一次性配对码状态 |
| OPTIONS | `/api/v1/browser-custody*` | 浏览器托管跨源预检 |
| GET | `/api/v1/browser-custody/status` | 脱敏托管状态 |
| POST | `/api/v1/browser-custody/capture` | 助手提交新的托管凭据 |
| DELETE | `/api/v1/browser-custody` | 助手撤销自身托管 |

配对端点依赖高熵一次性 token，并受 origin、过期时间、尝试次数和助手版本约束；它们不是匿名管理接口。

## 4. 总览、活动与控制

| 方法 | 路径 | 类别 | 说明 |
|---|---|---|---|
| GET | `/console/bootstrap` | RO | 控制台轻量启动数据 |
| GET | `/watchlist` | RO | 重点关注列表 |
| POST/PUT/DELETE | `/watchlist/items`, `/watchlist/items/{id}` | LR | 新增、修改、删除关注项 |
| PUT | `/watchlist/order` | LR | 保存手动排序 |
| GET/PUT | `/watchlist/settings` | RO/LR | 自动关注和展示数量 |
| GET/DELETE | `/activities` | RO/LR | 分页筛选或删除活动 |
| POST | `/activities/mirror-draft-batch` | LR | 保存批量草案结果摘要 |
| GET/PUT | `/automation-control` | RO/LR | 无人值守状态 |
| GET/PUT | `/automation-confirmation` | RO/LR | 自动化确认参数 |
| GET/PUT | `/automation-safety` | RO/LR | 自动化运行参数 |

## 5. 上游、凭据与采集

| 方法 | 路径 | 类别 | 说明 |
|---|---|---|---|
| GET/POST | `/suppliers` | RO/LR | 查询或创建上游 |
| PUT | `/suppliers/{id}` | LR | 修改上游 |
| GET/DELETE | `/suppliers/{id}/deletion-impact`, `/suppliers/{id}` | RO/LR | 删除预检和删除 |
| POST | `/suppliers/collect` | RR | 合并执行全部上游采集 |
| GET | `/collection-jobs/{id}` | RO | 查询采集任务 |
| POST | `/suppliers/{id}/collect` | RR | 采集单个上游 |
| GET | `/suppliers/{id}/snapshot` | RO | 最后可信快照 |
| GET | `/suppliers/{id}/prices` | RO | 标准化价格证据 |
| GET | `/suppliers/{id}/attempts` | RO | 分页采集尝试 |
| GET | `/suppliers/{id}/capabilities` | RO | 授权与采集能力证据 |
| GET | `/suppliers/{id}/credential-renewal-preset` | RO | 推荐续约参数 |
| POST | `/suppliers/{id}/login` | RR/LR | 执行声明式登录并更新加密凭据 |
| GET/DELETE | `/suppliers/{id}/browser-custody` | RO/LR | 管理托管状态 |
| GET/POST | `/credentials` | RO/LR | 查询或创建加密凭据 |
| GET/DELETE | `/credentials/{id}/deletion-impact`, `/credentials/{id}` | RO/LR | 凭据删除预检和删除 |
| POST | `/credentials/{id}/refresh` | RR/LR | 刷新凭据 |
| GET/PUT/DELETE | `/credentials/{id}/renewal` | RO/LR | 续约配置 |
| POST | `/credentials/{id}/renewal/trigger` | RR/LR | 手动触发续约 |
| POST | `/credentials/{supplierID}/pair` | LR | 生成一次性配对码 |

## 6. 本站目标控制

| 方法 | 路径 | 类别 | 说明 |
|---|---|---|---|
| GET/POST | `/targets` | RO/LR | 查询或创建目标站点 |
| POST | `/targets/connect` | RR/LR | 登录/Token 连接目标并读回能力 |
| PUT | `/targets/{id}` | LR | 修改目标配置 |
| GET/DELETE | `/targets/{id}/deletion-impact`, `/targets/{id}` | RO/LR | 删除预检和删除 |
| POST | `/targets/{id}/test` | RR | 读取控制面合同；不是模型测试 |
| GET | `/targets/{id}/ledger-sync` | RO | 账本同步状态 |
| PUT | `/targets/{id}/ledger-sync` | LR | 账本同步配置 |
| POST | `/targets/{id}/ledger-sync/run` | RR | 手动执行一次增量账本同步 |
| POST | `/targets/{id}/reauthorize` | RR/LR | 原子替换管理凭据 |
| GET | `/targets/{id}/capabilities` | RO | 目标能力矩阵 |
| GET | `/targets/{id}/operations` | RO | 控制面操作收据 |
| GET | `/targets/{id}/catalog` | RR | 共享缓存的目标价格目录 |
| GET | `/targets/{id}/channels` | RR | 共享缓存的渠道读回 |
| PUT | `/targets/{id}/group-ratios` | RW | 兼容入口，写入 GroupRatio |
| POST | `/targets/{id}/pricing-options/{option}/preview` | RR | 预览九类价格 Option |
| POST | `/targets/{id}/pricing-options/{option}/apply` | RW | 按 hash 写入单个 Option |
| POST | `/targets/{id}/channels/{channelID}/routing/preview` | RR | 路由预览 |
| POST | `/targets/{id}/channels/{channelID}/routing/apply` | RW | 路由写入和读回 |
| POST | `/targets/{id}/channels/{channelID}/status/preview` | RR | 启停预览 |
| POST | `/targets/{id}/channels/{channelID}/status/apply` | RW | 启停写入和读回 |
| POST | `/targets/{id}/channels/{channelID}/test` | BILL | 真实模型调用，可能收费 |

`{option}` 只允许：`GroupRatio`、`ModelRatio`、`ModelPrice`、`CompletionRatio`、`CacheRatio`、`CreateCacheRatio`、`ImageRatio`、`AudioRatio`、`AudioCompletionRatio`。

## 7. 价格映射与草案

| 方法 | 路径 | 类别 | 说明 |
|---|---|---|---|
| GET/POST | `/pricing-policies` | RO/LR | 价格规则查询和创建 |
| PUT/PATCH/DELETE | `/pricing-policies/{id}` | LR | 替换、开关或删除规则 |
| GET | `/pricing-policies/{id}/deletion-impact` | RO | 删除影响预检 |
| POST | `/pricing-policies/{id}/simulate` | RR | 使用可信快照模拟 |
| POST | `/pricing-policies/{id}/propose` | LR/RR | 生成变更提案 |
| POST | `/pricing-policies/bulk/preview` | RO | 批量规则预览 |
| POST | `/pricing-policies/bulk/apply` | LR | 批量规则本地落库 |
| POST | `/mapping-drafts/preview` | RO/RR | 自动生成价格/镜像草案 |
| POST | `/mapping-drafts/materialize` | LR | 将选中草案生成规则 |
| POST | `/mapping-drafts/manual/preview` | RO/RR | 手动对应关系预览 |
| POST | `/mapping-drafts/manual/materialize` | LR | 生成手动映射 |
| GET | `/change-proposals` | RO | 变更提案列表 |
| POST | `/change-proposals/{id}/apply` | RW | 应用并读回 |
| POST | `/change-proposals/{id}/reject` | LR | 拒绝提案 |
| POST | `/change-proposals/{id}/rollback` | RW | 条件回滚 |

## 8. 一键上架与分组镜像

| 方法 | 路径 | 类别 | 说明 |
|---|---|---|---|
| POST | `/listing-plans/preview` | RR | 生成一键上架计划，不做模型调用 |
| GET | `/listing-plans`, `/listing-plans/{id}` | RO | 查询计划 |
| POST | `/listing-plans/{id}/apply` | RW | 创建/更新渠道、价格合同和规则 |
| GET | `/listing-change-sets` | RO | 上架收据列表 |
| GET | `/listing-change-sets/{id}` | RO | 上架收据详情 |
| POST | `/listing-change-sets/{id}/rollback` | RW | 条件回滚上架 |
| GET/POST | `/mirror-policies` | RO/LR | 查询或创建镜像 |
| PUT/DELETE | `/mirror-policies/{uuid}` | LR | 修改或删除镜像 |
| POST | `/mirror-policies/{uuid}/preview` | RR | 冻结镜像预览 |
| GET | `/mirror-previews/{id}` | RO | 读取预览 |
| POST | `/mirror-previews/{id}/apply` | RW | 应用镜像并读回 |
| POST | `/mirror-adoptions/preview` | RR | 采用已有渠道的预览 |
| GET | `/mirror-adoption-previews/{id}` | RO | 读取采用预览 |
| POST | `/mirror-adoption-previews/{id}/apply` | RW | 应用采用预览 |
| GET | `/mirror-change-sets` | RO | 镜像变更列表 |
| GET | `/mirror-change-sets/{id}` | RO | 镜像变更详情 |
| GET | `/mirror-change-sets/{id}/events` | RO | 镜像逐步收据 |

## 9. 发现、经营与连接器配置

| 方法 | 路径 | 类别 | 说明 |
|---|---|---|---|
| GET/POST | `/discoveries`, `/discoveries/acknowledge` | RO/LR | 新品发现和确认 |
| GET | `/opportunities` | RO | 机会列表 |
| GET/PUT | `/opportunities/config` | RO/LR | 机会参数 |
| PUT | `/opportunities/usage` | LR | 保存已知用量 |
| GET | `/intelligence`, `/intelligence/overview` | RO | 经营详情和增量总览 |
| GET/PUT | `/intelligence/config` | RO/LR | 经营参数 |
| PUT | `/intelligence/ledger` | LR | 补充经营账本 |
| GET/POST | `/connector-specs` | RO/LR | JSON Connector 查询和创建 |
| PUT/DELETE | `/connector-specs/{id}` | LR | JSON Connector 修改和删除 |
| GET | `/connector-specs/{id}/deletion-impact` | RO | JSON Connector 删除影响 |
| GET/POST | `/connector-manifests` | RO/LR | Connector Manifest 查询和导入 |
| PUT/DELETE | `/connector-manifests/{id}` | LR | Connector Manifest 修改和删除 |
| GET | `/connector-manifests/{id}/deletion-impact` | RO | Manifest 删除影响 |
| GET | `/connector-manifests/{id}/export` | RO | 导出规范化 Manifest |
| GET/PUT | `/suppliers/{id}/connector-manifest` | RO/LR | 绑定或读取 Manifest |

## 10. 配置迁移

| 方法 | 路径 | 类别 | 说明 |
|---|---|---|---|
| GET | `/config/export` | RO | 导出不含明文凭据的配置包 |
| POST | `/config/imports/preview` | LR | 创建导入预览 |
| POST | `/config/imports/apply` | LR | 按 preview hash 应用 |
| GET | `/config/imports/{id}` | RO | 查询导入收据 |
| POST | `/config/imports/{id}/rollback` | LR | 条件回滚本地配置 |

## 11. 示例

```bash
curl -fsS http://127.0.0.1:18180/api/v1/suppliers \
  -H "X-RelayOps-Token: $RELAYOPS_ADMIN_TOKEN"
```

```bash
curl -fsS -X POST http://127.0.0.1:18180/api/v1/suppliers/1/collect \
  -H "X-RelayOps-Token: $RELAYOPS_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{}'
```

生产客户端必须设置连接、响应和总超时，并对 `409` 重新读取状态；不得自动重放 `RW` 或 `BILL` 请求。
