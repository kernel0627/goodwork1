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

## 2. ⚠️ 最重要的一条教训：爆炸半径只能从源码得到

本文件的第一版是照 flag 的名字和描述写的。**大约一半是错的。**

| flag | 从名字/描述推断 | 源码真相 |
|---|---|---|
| `cartFailure` | cart 服务失败 | **只作用于 `EmptyCart`**。实测 GetCart 0.67/s、AddItem 0.22/s、**EmptyCart 0.03/s** —— 故障的理论上限低于默认判定阈值，导致「注入满档也读作 inert」 |
| `imageSlowLoad` | image-provider 变慢 | **和 image-provider 无关**。浏览器组件读 flag 后设 `x-envoy-fault-delay-request` 头，由 **Envoy** 施加延迟。k6 只发 HTTP 请求不渲染页面，**合成流量大概打不到** |
| `recommendationCacheFailure` | 缓存失效导致延迟 | **是内存泄漏**。每次 miss 把 `cached_ids` 的 1/4 追加回自身，列表无界增长。类别应为 resource 而非 cache |
| `failedReadinessProbe` | 该服务请求出错 | 返回 `HealthCheckResult.Unhealthy`，**从不碰请求路径**。信号在 gRPC health 端点和容器健康状态，两者都不进 Prometheus |
| `paymentUnreachable` | payment 出错 | **checkout 读这个 flag** 并拒绝去连 payment。症状在调用方，被点名的服务只是「看起来不在了」 |
| `kafkaQueueProblems` | kafka 故障 | **checkout 是那个把队列压垮的生产者**，根因是 checkout 不是 kafka |

> **故障的爆炸半径和可观测签名，是「读这个 flag 的那段代码」的属性，不是 flag 名字的属性。**
> 每条 Spec 都记了 `Site` 字段指向源码位置，以后要质疑某个判定，从源码查，不要从直觉查。

这也正是**两级校验设计的价值** —— 它是唯一发现这些错误的机制。如果当初直接拿
「按名字猜出来的 catalog」去建场景库，会得到一批「看起来注入了其实没生效」的假场景，
后面所有数字都是脏的，而且**任何地方都不会报错**。

### 第二轮:校验实跑又推翻了两条

第一轮靠读源码纠正了 6 条。**实跑之后,又有两条被推翻** —— 而且两条都是被
校验器抓出来的,不是靠猜。

| flag | 实测 | 源码给出的定论 |
|---|---|---|
| `intlShippingSlowdown` | p99 前后都是 4.95ms | **合成流量下永远不可能触发。** shipping 只在地址国家**非美国**时延迟(`shipping_service.rs` 里判 `"US"｜"USA"｜"UNITED STATES"…`),而 `load-generator/people.json` 里**每一条记录的 country 都是 "United States"** |
| `kafkaQueueProblems` | 消费滞后前后都是 0 | **我的信号选错了。** variant `on: 100` 表示每单多产 100 条消息,flag 描述说的是 **"lag spike"** —— 消费者随后就追上了。用 instant query 在注入 150 秒后采样,**尖峰早就过去了**。改成 `max_over_time` 的窗口最大值 |

> **`kafkaQueueProblems` 这条是一般性教训:任何瞬时故障都必须用窗口最大值测,
> 不能用点采样。instant query 只能看到"此刻还在发生"的故障。**

`intlShippingSlowdown` 要变成可用,只有两条路:
改 SUT 的负载数据(**放弃可比性,不做**),或者**自己发起非美国地址的订单流量**
(可行,列为后续工作)。

### 合成流量打不到的故障

两条已标记 `SyntheticLoadReaches: false`，**不进场景库**（无论校验结果如何，
因为没有流量会触发它们）：

- `imageSlowLoad` —— 需要浏览器渲染 ProductCard
- `failedReadinessProbe` —— 需要容器级健康探针，Prometheus 里看不到

有单元测试 `TestFaultsUnreachableBySyntheticLoadAreFlagged` 钉住这两条，
上游若改了实现，测试会先叫。

---

## 3. 故障总表

难度定义：
- **L1 直接** — 症状服务就是根因服务，指标上一眼可见
- **L2 一跳** — 症状出现在调用方，根因在被调用方，需沿依赖链下探一层
- **L3 推理** — 症状形态与根因类型不一致（例如缓存失效表现为延迟而非错误），
  或跨异步边界，需要形成并验证假设

下表的「根因 / 症状 / 类别」三列**全部来自源码**（`Site` 列给出位置），
不是从 flag 名字推的。权威定义在 `gateway/internal/inject/catalog.go`，
本表与之同步；不一致时以代码为准。

| # | flag | 变体 | 类别 | 根因服务 | 症状服务 | 难度 | Site（源码位置） | 合成流量可达 | 验证状态 |
|---|---|---|---|---|---|---|---|---|---|
| 1 | `adFailure` | on | 错误率 | ad | ad | L2 | `ad/…/AdService.java:164` (getAds) | ✅ | ✅ 0→9.5% |
| 2 | `adHighCpu` | on | 资源 | ad | ad | L2 | `ad/…/AdService.java:166` | ✅ | ✅ 1.3→18.6 核 |
| 3 | `adManualGc` | on | 资源→延迟 | ad | ad | **L3** | `ad/…/AdService.java:165` | ✅ | ✅ p99 98→2485ms |
| 4 | `cartFailure` | 10–100% | 错误率 | cart | cart | L2 | `cart/…/CartService.cs:82` **仅 EmptyCart** | ✅ 但极弱 | ❌ INERT (trailer) |
| 5 | `emailMemoryLeak` | 1x–10000x | 资源 | email | email | **L3** | `email/email_server.rb:67` | ✅ | ✅ 内存 70→98% |
| 6 | `failedReadinessProbe` | on | 健康检查 | cart | cart | L1 | `cart/…/HealthCheckService.cs:36` | ❌ | ❌ UNREACHABLE |
| 7 | `imageSlowLoad` | 5/10sec | 延迟 | **frontend-proxy** | frontend-proxy | **L3** | `frontend/components/ProductCard/ProductCard.tsx:32` | ❌ | ❌ UNREACHABLE |
| 8 | `intlShippingSlowdown` | 5/10sec | 延迟 | shipping | shipping | **L3** | `shipping/src/shipping_service.rs:67` | **❌ 全是美国地址** | ❌ UNREACHABLE |
| 9 | `kafkaQueueProblems` | on | 队列/异步 | **checkout** | fraud-detection | **L3** | `checkout/main.go:707` | ✅ | 🔄 信号已改 `consumer_lag_max`，待重测 |
| 10 | `paymentFailure` | 10–100% | 错误率 | payment | **checkout**(调用方视角) | L2 | `payment/charge.js:39` | ✅ | ✅ 调用方错误比例 0→**1.0** |
| 11 | `paymentUnreachable` | on | 连通 | payment | **checkout** | L2 | `checkout/flags/flags_gen.go:51` | ✅ | ⬜ |
| 12 | `productCatalogFailure` | on | 错误率 | product-catalog | product-catalog | L2 | `product-catalog/flags/flags_gen.go:29` | ✅ 仅单个商品 | ⬜ |
| 13 | `recommendationCacheFailure` | on | **资源**（非缓存） | recommendation | recommendation | **L3** | `recommendation/recommendation_server.py:78` | ✅ | ⬜ |

变体列只列出用于校验的档位；完整可选值见 `injector list`。
`合成流量可达 = ❌` 的两条不进场景库（§2）。

### 实测的调用速率（决定各故障的信号上限）

| 端点 | 速率 |
|---|---:|
| `cart/GetCart` | 0.67 /s |
| `cart/AddItem` | 0.22 /s |
| `cart/EmptyCart` | **0.03 /s** ← `cartFailure` 的天花板 |

**判定阈值必须低于故障的信号上限**，否则满档注入也会读作 inert。
`cartFailure` 与 `productCatalogFailure` 因此在 catalog 里有
`MinDeltaOverride: 0.01`。

### 非故障开关（作为实验变量，不作为故障）

| flag | 变体 | 用途 |
|---|---|---|
| `loadGeneratorTraffic` | on/off（默认 on） | 需要静默环境时关掉 |
| `loadGeneratorVUs` | 5/10/25/50（默认 5） | 调负载。**注意：加负载本身会改变指标基线**，做对照时必须固定 |

---

## 4. 依赖关系（用于判定「症状 vs 根因」）

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

## 5. 场景库设计（T4）

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

## 6. 可观测签名 —— 实测的指标与 PromQL

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

1. **经验性地测出 flag → 服务的映射**，用来交叉核对从源码读出的 `Site`。
2. **确认确实有服务在读这个 flag**。没有服务读它，故障就不可能触发。

### 生效校验的两级判据

```
一级（必要）  有没有服务在评估这个 flag
              sum(rate(feature_flag_evaluation_requests_total{feature_flag_key="X"}[5m])) > 0

二级（充分）  预期信号是否发生偏移
              对应的错误率 / 延迟 / 资源 / 队列指标，注入前后差值 ≥ 阈值
```

两级都过才算场景有效。只过一级 = flag 读到了但没生效（inert）；
只过二级 = 症状可能来自噪声或上个场景的残留，归因给这个故障是错的（suspect）。

> ⚠️ **一级判据是静态属性，不是前后差值。** 我第一版写成「注入后评估计数有增长」，
> **那是错的**：load-generator 一直在跑，服务持续查 flag，计数在注入前就一直在涨，
> 所以「增长」恒为真，判据失效。正确的问题是「有没有任何服务在读它」。
> 代码里 `EvaluationQuery` 的注释记了这件事。

---

## 7. 待办

- [ ] 逐条注入验证，填「验证状态」列，记录每条故障在 Prometheus 上的**实际可观测签名**
- [ ] 确认 `failedReadinessProbe` 作用于哪个服务（配置里未直接体现，需实测）
- [ ] 确认 `intlShippingSlowdown` 的触发条件（疑似仅国际订单，需构造对应流量）
- [ ] 用 Jaeger 实际调用数据核对 §4 依赖图
- [ ] 测量各故障从注入到指标可见的**延迟**（决定 runner 的等待时长）
