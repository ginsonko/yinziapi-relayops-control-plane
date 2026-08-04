# 分组模型定价合同

## 1. 解决的问题

标准 NewAPI 的 `ModelRatio`、`CompletionRatio` 和 `ModelPrice` 通常按模型全局生效。当多个上游对同一模型给出不同价格时，仅靠全局模型价格无法让不同本站分组分别跟随各自上游。

RelayOps 的分组模型定价合同把基础价格作用域扩展为 `本站分组 + 模型`，同时保留 `GroupRatio` 作为最终利润倍率。该合同是可选的 NewAPI 控制面扩展，不是所有标准 NewAPI 实例都原生支持。

## 2. 四个核心字段

| 字段 | 作用域 | 语义 |
|---|---|---|
| `ModelRatio` | 模型或分组+模型 | token 输入基础倍率 |
| `CompletionRatio` | 模型或分组+模型 | 输出相对输入的倍率 |
| `ModelPrice` | 模型或分组+模型 | 按次、按秒或固定时长的基础价格 |
| `GroupRatio` | 用户分组 | 最终销售倍率/利润控制，最后乘算 |

分组镜像拥有模型集合、基础价格和计费单位；价格映射拥有 `GroupRatio`。两条规则不能同时写同一字段。

## 3. Token 模型公式

NewAPI 常用的基础口径是 `$2 / 1M tokens`。设：

- `m = ModelRatio`
- `c = CompletionRatio`
- `g = GroupRatio`

则：

```text
输入基础价 = 2 * m
输出基础价 = 2 * m * c
用户输入价 = 2 * m * g
用户输出价 = 2 * m * c * g
```

`CompletionRatio` 不是单独拿 `$2` 相乘，而是在 `ModelRatio` 之后相乘。

### $5 / $30 示例

```text
ModelRatio      = 2.5
CompletionRatio = 6

输入基础价 = 2 * 2.5     = 5
输出基础价 = 2 * 2.5 * 6 = 30
```

如果本站 `GroupRatio=1.2`，最终展示/扣费基础为 `$6 / $36`。如果希望用户仍看到 `$5 / $30`，该分组的 `GroupRatio` 应为 `1`。

可选的缓存、图片、音频倍率继续基于同一模型合同计算。RelayOps 使用十进制字符串进行计算和比较，避免浮点误差造成无限重复更新。

## 4. 固定价格模型

固定价格合同使用 `billing_mode=fixed_price`、`model_price` 和 `task_profile`。

### 按次

```json
{
  "billing_mode": "fixed_price",
  "model_price": 0.4,
  "task_profile": {"unit": "per_request"}
}
```

```text
用户单次基础价 = ModelPrice * GroupRatio
```

### 按秒

```json
{
  "billing_mode": "fixed_price",
  "model_price": 0.05,
  "task_profile": {"unit": "per_second"}
}
```

```text
用户价格 = ModelPrice * 实际计费秒数 * GroupRatio
```

实际秒数必须来自请求参数和 NewAPI 数据面的结算证据。RelayOps 只同步控制面价格合同，不参与请求预扣费、退款或时长结算。

### 固定时长套餐

```json
{
  "billing_mode": "fixed_price",
  "model_price": 2,
  "task_profile": {
    "unit": "fixed_duration",
    "fixed_duration_seconds": 15
  }
}
```

固定 15 秒套餐按整次 `ModelPrice * GroupRatio` 计费，不能再乘 15。`fixed_duration_seconds` 用于验证请求和向用户解释套餐，不等同于按秒计费。

## 5. 原子合同

```json
{
  "schema_version": 1,
  "revision": "sha256:server-generated-revision",
  "groups": {
    "premium": {
      "gpt-example": {
        "billing_mode": "ratio",
        "model_ratio": 2.5,
        "completion_ratio": 6,
        "source": "relayops:mirror/example",
        "source_revision": "catalog-revision-42",
        "source_observed_at_unix_ms": 1785772800000
      },
      "video-per-second": {
        "billing_mode": "fixed_price",
        "model_price": 0.05,
        "task_profile": {"unit": "per_second"}
      }
    }
  }
}
```

约束：

- `schema_version` 当前只支持 `1`；
- `ratio` 必须同时提供 `model_ratio` 和 `completion_ratio`，不得再提供 `model_price`；
- `fixed_price` 只提供 `model_price`，不得混入倍率字段；
- `per_request` 和 `per_second` 不得提供 `fixed_duration_seconds`；
- `fixed_duration` 必须提供 1 到 3600 秒的固定时长；
- 所有价格必须是有限、非负十进制数；
- 合同更新必须整体原子提交，不能留下半组旧价、半组新价；
- `revision` 由目标 NewAPI 根据规范化合同生成，RelayOps 不自称远端版本权威。

## 6. 镜像行为

完整分组镜像可以分别控制：

- 上游新增模型时加入本站受管渠道；
- 上游下架模型时从本站受管渠道移除；
- 同步分组逐模型基础价格；
- 同步 `per_request`、`per_second`、`fixed_duration` 等计费单位；
- 上游分组为空时移除所有受管模型；
- 对 `excluded_models` 中的模型不执行上架、下架或改价；
- 默认不修改 `GroupRatio`。

采集超时、DNS/TLS、429、5xx 或证据不完整时保留旧合同，不把“暂时没采到”解释为“上游已下架”。只有完整、权威且可解释的新目录才能生成镜像差异。

## 7. 兼容性回退

| 目标能力 | RelayOps 行为 |
|---|---|
| 支持分组模型合同 | 预览、CAS、原子写入、读回、条件回滚 |
| 仅支持全局价格 Option | 可以管理全局价格，但不能声称同一模型按分组独立定价 |
| 只支持模型集合 | 仅同步受管渠道模型集合，不写未知价格 |
| 未知计费单位或价格合同 | 停在预览，保留旧可信状态 |
| 写入后读回失败 | 标记结果不确定，不盲目重放写请求 |

绝不能把上游绝对美元价格直接写成 NewAPI 倍率，也不能把倍率当绝对价格。任何降级都必须在能力矩阵和预览中明确显示。

## 8. 验收清单

在启用自动化前至少验证：

1. `$5/$30` 能读回为 `2.5/6`，再按公式还原；
2. `GroupRatio` 变化不会改写基础模型合同；
3. 按秒模型分别用 1 秒和多秒请求验证真实结算；
4. 固定时长套餐不会重复乘时长；
5. 0 元维度保持 0，而不是被当作缺价；
6. 空分组只移除受管模型，不影响例外模型；
7. 网络失败和证据不完整不产生下架计划；
8. 并发管理员修改会触发 revision 冲突而不是被覆盖；
9. 应用后读回 revision 与预览 desired revision 完全一致；
10. 条件回滚只在目标仍保持本次写入结果时执行。
