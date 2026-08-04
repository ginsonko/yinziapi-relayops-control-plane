# NewAPI 目标集成合同

## 1. 集成层次

RelayOps 对 NewAPI 目标分为三层能力：

1. 标准 Option：`GroupRatio`、`ModelRatio`、`ModelPrice`、`CompletionRatio`、`CacheRatio`、`CreateCacheRatio`、`ImageRatio`、`AudioRatio`、`AudioCompletionRatio`；
2. 渠道控制：目录、渠道增删改查、模型集合、路由、启停与读回；
3. 可选扩展：分组逐模型原子价格合同。

目标适配器位于 `internal/targets`。基础接口在 `newapi.go`，分组模型实现位于 `group_model_pricing.go`，合同类型位于 `internal/domain/operations.go`。

## 2. 标准 NewAPI 接口

RelayOps 使用管理员控制面凭据，并按目标能力协商以下常见接口：

| 功能 | 常见路径 |
|---|---|
| 状态/计价口径 | `/api/status`, `/api/setting` |
| 价格目录 | `/api/pricing` |
| Option 读取/写入 | `/api/option/` |
| 渠道列表/更新 | `/api/channel/`, `/api/channel/{id}` |
| 渠道模型读取 | `/api/channel/fetch_models/{id}` |
| 渠道启停 | `/api/channel/{id}/status` |

常见认证 Header：

```http
Authorization: Bearer <management-token>
New-Api-User: <administrator-user-id>
```

魔改分支如果只改变路径或 Header，应优先使用 Connector Manifest 或扩展 NewAPI Target Adapter 的路径配置，不要在通用解析器中堆叠站点名称判断。

## 3. 分组模型合同扩展

该扩展不是标准 NewAPI 必备接口。RelayOps 会先读取能力；`404` 或 `405` 表示目标不支持，随后只使用目标明确提供的标准能力。

### 读取

```http
GET /api/option/group-model-pricing
```

```json
{
  "success": true,
  "data": {
    "supported": true,
    "schema_version": 1,
    "revision": "sha256:current",
    "contract": {
      "schema_version": 1,
      "revision": "sha256:current",
      "groups": {}
    }
  }
}
```

### 预览

```http
POST /api/option/group-model-pricing/preview
Content-Type: application/json
```

```json
{
  "expected_revision": "sha256:current",
  "contract": {
    "schema_version": 1,
    "groups": {
      "premium": {
        "gpt-example": {
          "billing_mode": "ratio",
          "model_ratio": 2.5,
          "completion_ratio": 6
        }
      }
    }
  }
}
```

服务端必须规范化十进制、字段顺序和空值，计算 desired revision，并返回：

```json
{
  "success": true,
  "data": {
    "supported": true,
    "schema_version": 1,
    "current_revision": "sha256:current",
    "revision": "sha256:desired",
    "expected_revision_matches": true,
    "changed": true,
    "contract": {
      "schema_version": 1,
      "revision": "sha256:desired",
      "groups": {
        "premium": {
          "gpt-example": {
            "billing_mode": "ratio",
            "model_ratio": 2.5,
            "completion_ratio": 6
          }
        }
      }
    }
  }
}
```

预览是语义读取，不得持久化或修改计费状态。

### 原子更新

```http
PUT /api/option/group-model-pricing
Content-Type: application/json
```

请求体与预览相同。服务端必须在一个事务内：

1. 锁定当前合同；
2. 重新计算当前 revision；
3. 比较 `expected_revision`；
4. 验证完整 desired contract；
5. 原子替换合同并提交；
6. 返回持久化后的规范化合同及新 revision。

revision 不一致应返回 `409 Conflict`，不能覆盖管理员的并发修改。验证失败应返回 `400/422`；权限不足返回 `401/403`；临时数据库繁忙可以返回 `429/503` 并附带安全、脱敏的错误分类。

## 4. 服务端不变量

- 同一 revision 必须始终对应同一规范化合同；
- map 顺序、数字尾零和 JSON 空白不能改变 revision；
- 所有组和模型条目一次提交，不允许部分成功；
- 0 是合法价格，缺失字段与 0 必须区分；
- 未知 `billing_mode` 或 task unit 必须拒绝；
- 服务器不得信任客户端提供的 desired revision；
- 更新响应成功后，独立 GET 必须读到相同 revision；
- 数据面读取合同应使用原子快照，不能观察到半更新状态；
- 日志不得包含管理员 Token、Cookie 或合同来源响应正文。

## 5. RelayOps 写入序列

```text
GET current
-> POST preview(expected_revision, desired)
-> acquire local lease/fencing token
-> re-check preview/current revision
-> PUT atomic update
-> independent GET readback
-> save change-set receipt
```

如果 PUT 超时，RelayOps 先 GET 读回：已是 desired revision 则判定成功；仍是 expected revision 且收到明确拒绝才判定未写入；无法证明时标记 `ambiguous_after_write`，不会盲目重放。

回滚同样要求当前 revision 仍等于本次写入结果。检测到其他修改后停止回滚，以免覆盖管理员变更。

## 6. 没有扩展接口时

标准 NewAPI 仍可使用全局 Option 和渠道能力，但存在边界：

- 同一模型不能在两个本站分组拥有不同基础价格；
- `per_second` 和 `fixed_duration` 若没有可证明的数据面合同，不得模拟；
- RelayOps 可以只同步受管渠道的模型集合；
- 分组镜像预览应把逐模型定价标记为不可用，而不是写默认 `1`、默认价格或全局覆盖值。

要获得完整能力，可以在 NewAPI 分支实现上述三个端点，或实现另一个满足 `targets.GroupModelPricingAdapter` 的目标适配器。

## 7. Sub2API 目标

本仓库已内置 Sub2API 的只读 Source Connector，但尚未内置 Sub2API Target Adapter。实现顺序见[连接器与目标适配器开发指南](CONNECTOR_DEVELOPMENT.md)。若 Sub2API 不提供原子逐模型合同，只能声明其真实支持的子能力，不能复用 Source Connector 假装写入成功。

## 8. 合同测试

`internal/targets/group_model_pricing_test.go` 提供可执行的目标端模拟，覆盖读取、预览、应用、revision 冲突、瞬时失败恢复、读回和条件回滚。接入新的 NewAPI 分支时，应先让这些测试对其协议 fixture 通过，再连接一次性目标实例。
