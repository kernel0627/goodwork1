# 故障清单

> 来源：`sut/opentelemetry-demo/src/flagd/demo.flagd.json`（实地读取，15 个 flag）
> 注入方式：**原子改写该 JSON 文件**（flagd 以 `file:` URI 监听 bind mount），
> 详见 [`docs/sut.md`](./sut.md) §3。
>
> ⚠️ **本表的「预期信号」列目前是从服务依赖关系推断的，尚未逐条实测。**
> T3 的收尾工作就是逐条注入并核对信号，把「验证状态」列填成实测结果。
> 未验证的故障不得进入场景库。

---

## 1. 为什么这套开关够用

不需要自己写 `tc netem` / `iptables` / cgroup 注入，原因有三：

1. **7 类故障模式**已经齐了（错误率 / 延迟 / 资源 / 连通 / 健康检查 / 队列 / 缓存）。
2. **有分级严重度**：`cartFailure` 可设 10%–100%，`emailMemoryLeak` 可设 1x–10000x，
   延迟类可设 5s / 10s。这让「Agent 能不能察觉弱信号」成为可测的问题，
   而不是只有「全崩 / 全好」两档。
3. **多数故障的症状服务 ≠ 根因服务**（见下表「归因难度」），
   天然产生误导性场景——这正是区分「多步推理」与「看一眼指标」的地方。

---

## 2. 故障总表

难度定义：
- **L1 直接** — 症状服务就是根因服务，指标上一眼可见
- **L2 一跳** — 症状出现在调用方，根因在被调用方，需沿依赖链下探一层
- **L3 推理** — 症状形态与根因类型不一致（例如缓存失效表现为延迟而非错误），
  或跨异步边界，需要形成并验证假设

| # | flag | 变体 | 类别 | 根因服务 | 预期症状出现在 | 归因难度 | 验证状态 |
|---|---|---|---|---|---|---|---|
| 1 | `adFailure` | on/off | 错误率 | ad | frontend（广告位报错） | L2 | ⬜ 待验证 |
| 2 | `adHighCpu` | on/off | 资源 | ad | ad 延迟↑，frontend 长尾 | L2 | ⬜ 待验证 |
| 3 | `adManualGc` | on/off | 资源→延迟 | ad | ad 周期性延迟毛刺 | **L3** | ⬜ 待验证 |
| 4 | `cartFailure` | 10/25/50/75/90/100% | 错误率 | cart | frontend、checkout | L2 | ⬜ 待验证 |
| 5 | `emailMemoryLeak` | 1x/10x/100x/1000x/10000x | 资源 | email | email OOM/重启，checkout 尾部失败 | **L3** | ⬜ 待验证 |
| 6 | `failedReadinessProbe` | on/off | 健康检查 | **cart**（flag 描述明确写了 "for cart service"） | cart 被判不健康 | L1 | ⬜ 待验证 |
| 7 | `imageSlowLoad` | 5sec/10sec | 延迟 | image-provider | frontend 页面加载慢 | L2 | ⬜ 待验证 |
| 8 | `intlShippingSlowdown` | 5sec/10sec | 延迟 | shipping | checkout P99↑（**仅国际订单**，条件触发） | **L3** | ⬜ 待验证 |
| 9 | `kafkaQueueProblems` | on/off | 队列/异步 | kafka | accounting、fraud-detection 消费滞后 | **L3** | ⬜ 待验证 |
| 10 | `paymentFailure` | 10/25/50/75/90/100% | 错误率 | payment | checkout 下单失败 | L2 | ⬜ 待验证 |
| 11 | `paymentUnreachable` | on/off | 连通 | payment | checkout 连接错误 | L2 | ⬜ 待验证 |
| 12 | `productCatalogFailure` | on/off | 错误率 | product-catalog | frontend **和** recommendation（扇出） | L2 | ⬜ 待验证 |
| 13 | `recommendationCacheFailure` | on/off | 缓存 | recommendation | recommendation 延迟↑ 而非报错 | **L3** | ⬜ 待验证 |

### 非故障开关（作为实验变量，不作为故障）

| flag | 变体 | 用途 |
|---|---|---|
| `loadGeneratorTraffic` | on/off（默认 on） | 需要静默环境时关掉 |
| `loadGeneratorVUs` | 5/10/25/50（默认 5） | 调负载。**注意：加负载本身会改变指标基线**，做对照时必须固定 |

---

## 3. 依赖关系（用于判定「症状 vs 根因」）

从 compose 与服务定义推断，**待用 Jaeger 实际调用链核对**：

```
frontend ──┬─▶ product-catalog ◀── recommendation
           ├─▶ recommendation
           ├─▶ ad
           ├─▶ image-provider
           ├─▶ cart ─▶ valkey-cart
           └─▶ checkout ──┬─▶ cart
                          ├─▶ payment
                          ├─▶ shipping
                          ├─▶ currency
                          ├─▶ email
                          ├─▶ product-catalog
                          └─▶ kafka ─┬─▶ accounting
                                     └─▶ fraud-detection
```

**`get_service_topology` 工具应当由 Jaeger 的实际调用数据生成，而不是硬编码这张图。**
硬编码等于把答案喂给 Agent，消融「关掉拓扑工具」时就不公平了。

---

## 4. 场景库设计（T4）

计划 40–60 个场景，配比：

| 类型 | 占比 | 说明 |
|---|---|---|
| L1 直接 | ~15% | 保底，验证基本能力 |
| L2 一跳 | ~40% | 主力 |
| L3 推理 | ~30% | **区分度来源**，也是 baseline 最容易输的地方 |
| 多故障并发 | ~10% | 两个 flag 同时开，测能否分清主次 |
| **无故障对照** | ~5% | **必须有**。测 Agent 会不会在系统健康时凭空捏造根因并乱动手 |

每个场景需声明：注入组合、严重度、持续时间、根因标签（服务 + 类别）、
恢复判据（健康探针 / SLO 阈值）、预期观测信号（用于生效校验）。

---

## 5. 可观测签名 —— 实测的指标与 PromQL

Prometheus 里共 **524 个指标**。验证故障是否生效、以及 Agent 诊断时要查什么，
都落在下面这几组上。

### 关键标签（实测）

| 指标 | 关键标签 | 用途 |
|---|---|---|
| `rpc_server_duration_milliseconds_count` | **`rpc_grpc_status_code`**、`service_name`、`rpc_method`、`rpc_service` | gRPC 错误率与调用量。`status_code != "0"` 即错误 |
| `rpc_server_duration_milliseconds_bucket` | 同上 + `le` | 延迟分位数 |
| `http_server_*` / `http_client_*` | `service_name`、`http_*` | HTTP 侧同理 |
| `container_cpu_utilization_ratio` | 容器标签 | 资源类故障（`adHighCpu`） |
| `container_memory_percent_ratio` | 容器标签 | 资源类故障（`emailMemoryLeak`） |
| `kafka_consumer_records_lag` / `kafka_consumer_group_lag_*` | consumer group | 队列类故障（`kafkaQueueProblems`） |
| **`feature_flag_evaluation_requests_total`** | **`feature_flag_key`**、`service_name` | 见下 |

### `feature_flag_evaluation_requests_total` 是生效校验的最强判据

flagd 的 OTel 遥测带 `feature_flag_key` 和 `service_name` 两个标签。这给了两个能力：

1. **经验性地测出 flag → 服务的映射**，不用从 flag 描述文字猜。
   §2 表里「根因服务」一列应当用这个指标核对，而不是信描述。
2. **确认某服务确实在评估这个 flag**。注入后如果该 flag 的评估计数没有增长，
   说明根本没有服务在读它 —— 场景无效，直接作废。

> 注意：实测当前只有 `cartFailure` 和 `failedReadinessProbe` 两个 key 有数据，
> 因为只有真正去查 flag 的服务才会上报。这本身就是信息：
> **注入前后对比这个指标，才知道注入通道是否打通。**

### 生效校验的两级判据

```
一级（必要）：flagd 侧    feature_flag_evaluation_requests_total{feature_flag_key="X"} 有增长
二级（充分）：下游症状    对应的错误率 / 延迟 / 资源指标出现预期偏移
```

两级都过才算场景有效。只过一级说明 flag 读到了但没生效；
只过二级说明症状可能来自别的原因（噪声、其他故障残留）。

---

## 6. 待办

- [ ] 逐条注入验证，填「验证状态」列，记录每条故障在 Prometheus 上的**实际可观测签名**
- [ ] 确认 `failedReadinessProbe` 作用于哪个服务（配置里未直接体现，需实测）
- [ ] 确认 `intlShippingSlowdown` 的触发条件（疑似仅国际订单，需构造对应流量）
- [ ] 用 Jaeger 实际调用数据核对 §3 依赖图
- [ ] 测量各故障从注入到指标可见的**延迟**（决定 runner 的等待时长）
