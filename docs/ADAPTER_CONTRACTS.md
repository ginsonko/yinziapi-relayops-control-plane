# 适配器合同参考

## 1. Source Connector

最小只读接口：

```go
type Connector interface {
    Name() string
    Collect(ctx context.Context, supplier domain.Supplier) (domain.CatalogSnapshot, error)
}
```

需要请求级证据时实现：

```go
type DetailedConnector interface {
    Connector
    CollectDetailed(
        ctx context.Context,
        supplier domain.Supplier,
    ) (domain.CatalogSnapshot, domain.CollectionDiagnostics, error)
}
```

Source Connector 的输出是标准化目录和脱敏证据。它不能写远端价格、渠道或分组，不能把响应正文、Header、Cookie、Token 或带查询参数的 URL 放入诊断。

注册入口为 `connectors.Factory.ForSupplier`。声明式扩展应优先使用 Connector Manifest 或 JSON Connector，只有无法表达鉴权、分页或计费语义时才新增原生 Go Connector。

## 2. Target Adapter

基础写侧接口：

```go
type Adapter interface {
    Name() string
    Snapshot(context.Context, domain.TargetSite) (domain.TargetSnapshot, error)
    ApplyGroupRatio(
        context.Context,
        domain.TargetSite,
        string,
        map[string]string,
    ) (domain.TargetSnapshot, error)
    RollbackGroupRatio(
        context.Context,
        domain.TargetSite,
        string,
        map[string]string,
    ) (domain.TargetSnapshot, error)
}
```

可选能力通过小接口声明：

| 接口 | 能力 |
|---|---|
| `PricingOptionAdapter` | 写入单个全局价格 Option |
| `GroupModelPricingAdapter` | 分组逐模型合同的读取、预览、应用和回滚 |
| `ChannelAdapter` | 渠道目录、创建、查找和生命周期 |
| `ManagedChannelStateAdapter` | 受管渠道模型集合的 revision/CAS 更新 |
| `GroupVisibilityAdapter` | 用户可见分组读写 |
| `ControlledAdapter` | 统一能力矩阵和带 fencing guard 的控制操作 |

不支持某接口时必须返回 unavailable，不能返回空成功结果。

## 3. 能力与副作用

| 操作 | 类别 | 要求 |
|---|---|---|
| 读取本地快照 | RO | 不访问远端 |
| 采集上游/读取目标 | RR | 有界超时、无模型调用费用 |
| 修改本地规则 | LR | SQLite 事务和幂等键 |
| 修改目标控制面 | RW | 远程写开关、预览、CAS、读回、收据 |
| 真实模型测试 | BILL | 独立显式操作，不得成为上架门禁 |

一键上架、映射草案和分组镜像不得偷偷调用 BILL 操作。

## 4. 错误分类

| 类别 | 适配器行为 | 上层行为 |
|---|---|---|
| DNS/TLS/超时/连接重置 | transient | 保留旧快照，按计划重试 |
| 429/上游 5xx | transient | 保留旧快照，不产生配置失败风暴 |
| 401/403 | definitive authorization | 标记管理员关注，不覆盖凭据 |
| 404/405 能力端点 | unsupported | 能力矩阵关闭该子能力 |
| 未知字段/单位/价格 | semantic rejection | 停止相关写入并给出结构化诊断 |
| revision 不一致 | conflict | 重新读取和生成预览 |
| 写请求超时 | ambiguous | 先独立读回，不盲目重放 |

错误字符串在进入日志前必须脱敏。网络错误无论重复多少次都不能覆盖最后成功快照。

## 5. 快照合同

连接器输出的 `CatalogSnapshot` 至少应满足：

- 分组和模型标识稳定；
- 同一模型在不同上游分组中保留独立记录；
- 价格使用十进制字符串；
- 0 与缺失值严格区分；
- 币种、billing mode 和 unit 可解释；
- evidence 包含观测时间、来源类型和语义响应 hash；
- 分页未完成、条目丢弃或单位歧义时不得声称目录权威完整。

只有完整、权威的新快照可以触发模型下架或批量价格写入。

## 6. 写入合同

Target Adapter 的写操作必须遵循：

```text
read current
-> normalize desired
-> preview diff
-> compare expected revision/hash
-> acquire local lease and fencing token
-> apply one atomic target mutation
-> independent readback
-> persist receipt
```

条件回滚只在远端仍等于本次 desired state 时执行。检测到管理员或另一个 worker 的修改后停止回滚。

## 7. Sub2API Target Adapter 最小实现

当前仓库没有内置 Sub2API 写侧实现。二次开发时按真实能力逐步增加：

1. `Snapshot` 与基础 `GroupRatio` 读写；
2. 单 Option 的预览、CAS 和读回；
3. 渠道生命周期；
4. 如果服务端提供原子合同，再实现分组逐模型定价；
5. 在 `targets.Factory.ForTarget` 注册新 adapter 名；
6. 用 disposable Sub2API fixture 覆盖冲突、部分失败和条件回滚。

如果 Sub2API 没有精确表达某个字段的接口，能力矩阵就应保持 unavailable。

## 8. 合同测试最低集合

- 正常目录、空分组、分页和重复项；
- 0 元、token、按次、按秒、固定时长；
- 401/403、429、5xx、超时和响应截断；
- 未知字段、未知单位和币种混合；
- 语义相同但 JSON 顺序/小数尾零不同；
- revision 冲突、幂等重放和写后读回失败；
- 日志与错误中不出现凭据和响应正文；
- 远程写关闭时请求不会到达目标。
