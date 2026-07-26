# STATUS

> 本文件是项目的唯一进度真源。每完成一步就更新。
> 看这一个文件应该能知道：现在在哪、下一步做什么、为什么这么决定、什么卡住了。

**最后更新**：2026-07-26

---

## 当前阶段

**阶段 0 — 项目骨架与环境打通**

---

## 硬约束（不可违反）

1. 所有文件工作**只在 `/Users/traegang/Code/fine/1/`** 内进行，不碰任何其他目录。
2. Python 一律使用 `/Users/traegang/miniforge3/envs/agent/bin/python`（conda `agent` 环境）。
3. Docker 容器/镜像/卷是全局资源，无法限制在目录内。**所有本项目容器与卷统一加 `medic-` 前缀**，
   `make clean-docker` 可一键清除。这是唯一的目录外留痕。

---

## 环境事实

| 项 | 状态 |
|---|---|
| Go | 1.26.4 darwin/arm64 ✅ |
| Docker | 29.6.1 ✅（**daemon 未启动 ⛔**） |
| Docker Compose | v5.1.4 ✅ |
| git | 2.55.0 ✅ |
| 主机 | 24 GB RAM / 10 核 / 244 GB 可用 ✅（OTel Demo 约需 6 GB，够） |
| Python | 3.10.20 @ conda `agent` ✅ |
| Python 依赖 | ✅ 已装入 conda `agent`：fastapi 0.140.0 / uvicorn 0.51.0 / anthropic 0.120.0 / python-dotenv 1.2.2（其余环境已有）。版本已钉入 `agent/requirements.txt` |
| Docker VM 内存 | **7.75 GiB** ⚠️ OTel Demo 全量约需 6 GB，可跑但不宽裕 |
| SUT | ✅ 已拉取 `opentelemetry-demo` @ `1755859`（2026-07-24 release） |

---

## 关键决策记录

记在这里是为了**不再重复讨论**。要推翻某条决策，在此追加而不是删除。

| # | 决策 | 理由 | 日期 |
|---|---|---|---|
| D1 | 项目形态 = 真实系统上的诊断与处置 Agent，而非虚构业务域 Agent | 前期反复否掉客服/计费/售后类方案，根因是「世界是编的」。诊断类的 Agent 身份对应真实岗位（oncall），存在理由自洽 | 07-26 |
| D2 | 被测系统用 **OpenTelemetry Demo**，不自建、不用 train-ticket | 15 个多语言真实服务；docker compose 一键起；Prometheus/Jaeger/日志**预接好**；**自带 feature-flag 故障注入**。一次性消除最大的时间黑洞（可观测栈配置）。train-ticket 41 个 Java 服务太重 | 07-26 |
| D3 | 语言边界：**Go 管副作用，Python 管推理** | Gateway/Injector/Runner/判分 → Go；诊断编排与 LLM → Python | 07-26 |
| D4 | Runner 用 Go，Agent 作为 Python HTTP 服务被调用 | 判分要读系统真实状态、注入与复原是副作用，都属 Go 侧；并发跑场景 Go 更顺 | 07-26 |
| D5 | Enforcing 模式常开，消融时只关 Advisory | 真实系统不能真越权。指标因此叫「**越权尝试率**」，由 Gateway 拦截记录得出 | 07-26 |
| D6 | 先用**随机动作 Agent** 验证判分框架，再写真 Agent | 防止判分逻辑自身有 bug 却被误当成 Agent 表现 | 07-26 |
| D7 | 不上 K8s、不做符号级可达性、不自建可观测栈、不做多 Agent | 范围控制，写进 design.md 的非目标 | 07-26 |
| D8 | 故障注入 = **原子改写 `src/flagd/demo.flagd.json`** | flagd 以 `file:` URI 监听 bind mount，改文件即热加载。不需要 API、不需 exec 进容器、不需重启服务。**v1 不写 tc/iptables/cgroup 注入**——13 个 flag 已覆盖 7 类故障模式且带分级严重度 | 07-26 |
| D9 | 用本项目自己的 `sut/compose.medic.yaml` overlay 放开 Prometheus 9090 | 上游既无 ports 映射也无 Envoy 路由，PromQL 打不到。**绝不修改 SUT 仓库文件**，overlay 保持最小（只发端口），否则被测系统被改了、结果不可比 | 07-26 |
| D10 | **不启用 `compose.agent.yaml`** | 上游自带 agent/mcp/chatbot 三个 AI 服务，会污染观测面并混淆根因归因 | 07-26 |
| D11 | `get_service_topology` 必须由 **Jaeger 实际调用数据**生成，不得硬编码依赖图 | 硬编码等于把答案喂给 Agent，消融「关掉拓扑工具」时不公平 | 07-26 |
| D12 | **28 个容器不可裁剪，全部保留**（此条已修正一次，见下） | `docker compose config` 实测：`frontend-proxy → flagd-ui grafana telemetry-docs jaeger opamp-server frontend`；`otel-collector → opensearch jaeger opamp-server`。frontend-proxy 是唯一宿主机入口，即使 `up` 时只列想要的服务，compose 也会按 depends_on 补齐。要砍必须 `!override` 改写 depends_on = **修改被测系统** = 结果失去可比性。4.3 GB / 7.75 GiB 有 3.4 GB 余量，够用 | 07-26 |
| D12a | **依赖关系一律用 `docker compose config` 取，不要自己解析 YAML** | 我最初手写脚本逐文件解析，让后面的文件**覆盖**了 `depends_on`，而 compose 实际是**合并**，于是漏掉 frontend-proxy 对 flagd-ui/telemetry-docs 的依赖，得出「能砍 2 个」的错误结论。`make sut-deps` 就是这个用途 | 07-26 |
| D13 | `injector set` 是**绝对语义而非增量**：提交的就是命令行上列出的故障集，未列出的一律回退到 baseline | 增量语义会让故障跨场景累积，第一个场景之后全部被污染 | 07-26 |
| D14 | `query_logs` 走**容器日志**（compose 已配 json-file driver），不依赖 opensearch | 更贴近真实排障动作；且不受 collector 的 logs pipeline 是否健康影响 | 07-26 |
| D15 | ⚠️ **不要试图用 `settings-store.json` 改 Docker VM 内存** | 实测：写入 `MemoryMiB`/`Cpus` 后 Docker Desktop 会**自行剥掉这两个 key**（不认这个 schema），且该次重启 VM 未能起来（`no route to host` 到 192.168.65.7:2376）。要调内存只能走 Docker Desktop 的 GUI 设置。已回滚，文件恢复原状 | 07-26 |

---

## 进度看板

### ✅ 已完成

- [x] 环境探测（Go / Docker / git / 内存 / conda python）
- [x] `docs/design.md` — 架构、Go/Python 边界、四层判分 Oracle、对照组与消融、非目标
- [x] `.gitignore` / `.env.example` / `README.md` / `STATUS.md`
- [x] **T1 项目骨架**
  - 目录树：`docs/ sut/ scenarios/ gateway/{cmd,internal} agent/medic/{tools,agents,llm,trace} results/`
  - `gateway/go.mod`（module `github.com/kernel0627/medic/gateway`, go 1.26）
  - `Makefile`：`doctor / pydeps / sut-fetch / sut-up / sut-ps / build / test / clean / clean-docker`
  - `agent/requirements.txt`
  - `gateway/cmd/probe` — **可观测面探针**，5 项检查：前端可达、Prometheus 健康、
    scrape targets 上下线数、PromQL 能查到 duration 直方图、Jaeger 服务列表。
    `go vet` + `go build` 通过。
  - `git init` 完成（**尚未提交，等确认**）

- [x] **环境解阻塞**：Docker daemon 已启动；Python 依赖已装入 conda `agent`
- [x] **SUT 勘察** → `docs/sut.md`
  - 上游已改为 `compose.yaml` + 5 个 overlay；`make start` 实为 CORE+FULL+OBSERVABILITY+EXTRAS，
    且**两个 `--env-file` 缺一不可**
  - **发现坑：Prometheus 无 host 端口也无 Envoy 路由**，PromQL 打不到 → 写了
    `sut/compose.medic.yaml` overlay 放开 9090（不改 SUT 仓库）
  - Envoy 路由前缀实地读出：`/ /jaeger/ /grafana/ /feature /flagservice/ /telemetry/ /images/ ...`
  - `make sut-config` 干跑校验通过：**28 个服务**可解析，overlay 被接受
- [x] **故障能力勘察** → `docs/faults.md`
  - `demo.flagd.json` 里 **15 个 flag，13 个是故障注入**
  - 覆盖 **7 类故障模式**：错误率 / 延迟 / 资源 / 连通 / 健康检查 / 队列异步 / 缓存
  - **带分级严重度**：`cartFailure` `paymentFailure` 各 6 档百分比；
    `emailMemoryLeak` 5 档倍率；延迟类 5s/10s → 可测「Agent 能否察觉弱信号」
  - 已按 L1 直接 / L2 一跳 / L3 推理 标注归因难度，L3 共 5 条（区分度来源）
  - 已排出场景库配比方案（含 **5% 无故障对照**，测 Agent 会不会凭空捏造根因）
- [x] `agent/requirements.txt` 钉版本（实测版本 + 注释说明用途）
- [x] `Makefile` 补 `sut-config / sut-health / probe`，`sut-up` 改为正确的完整 compose 调用

- [x] **T2 跑通 OTel Demo — ✅ probe 5/5 全绿，项目最大技术风险已解除**
  - 28 个容器全部 Up
  - 探针实测：前端 200 / Prometheus healthy / **22 个 service_name 上报** /
    **23 个 job 有 duration 直方图** / **Jaeger 19 个 service**
  - 修正两处**探针自身的错误假设**（不是 SUT 的问题）：
    - ⚠️ **Prometheus 无 scrape target**：以 `--web.enable-otlp-receiver` 启动，
      指标由 collector 推送，`/api/v1/targets` 恒为 0/0。改查
      `/api/v1/label/service_name/values`
    - ⚠️ **Jaeger base path 是 `/jaeger/ui` 而非 `/jaeger`**：
      `/jaeger/api/services` → 404，`/jaeger/ui/api/services` → 200
  - 两条修正已落 `docs/sut.md`，并对 T5 工具层同样生效
- [x] **T4 的一半：故障注入器（Go）已实现并测试通过**
  - `gateway/internal/inject` + `testdata/demo.flagd.json` fixture
  - **抗漂移设计**：绝不原地改文件。首次运行快照 pristine baseline，
    此后每次提交 = baseline + 当前生效故障集。跑几百个场景不会累积漂移，
    reset 精确复原
  - **原子写**（temp + rename），flagd 不会读到半个文件
  - **处理了 targeting 陷阱**：`productCatalogFailure` 的 JsonLogic 规则会压过
    `defaultVariant`，只翻 default 是静默无效的注入。现在会重写规则的 then 分支
    并保留 condition（故障范围不变）
  - **输入校验**：未知 flag / 未知 variant 直接报错。拼错导致「看起来注入了其实没有」
    是评测最致命的失败模式
  - 10 个测试全过，其中 **`TestRoundTripIsByteStable`（空提交必须字节级复现原文件）**
    和 **`TestCommitIsIdempotentAcrossRuns`（同一故障集在任意历史后产出相同字节）**
    是承重测试

- [x] **注入器 CLI**（`gateway/cmd/injector`）：`list / set / reset / status`，端到端实测通过
  - `set` **绝对语义**：提交的就是命令行列出的故障集，未列出的回退 baseline
  - 区分「故障」与「实验变量」（`loadGenerator*`）—— 后者会移动指标基线，
    必须在各对照组间固定，不能当故障注入
  - 标出带 targeting 的 flag；拼错 variant 会报错并列出合法值
  - 从工作目录向上找 flag 文件，仓库任意位置都能跑
- [x] **容器精简结论已推翻并纠正**（见 D12 / D12a）：28 个不可裁剪，
  `make sut-deps` 现在打印权威合并依赖
- [x] **Docker VM 内存调整失败并已回滚**（见 D15）：这条路走不通，
  要调只能走 GUI。7.75 GiB 对 4.26 GB 占用够用（`make sut-mem` 实测合计 4261 MiB / 28 容器）
- [x] **可观测签名勘察** → `docs/faults.md` §5
  - Prometheus 共 **524 个指标**
  - 错误率判据：`rpc_server_duration_milliseconds_count{rpc_grpc_status_code!="0"}`
  - 延迟：同名 `_bucket` + `le`；资源：`container_cpu_utilization_ratio`、
    `container_memory_percent_ratio`；队列：`kafka_consumer_records_lag`
  - **最重要的发现**：`feature_flag_evaluation_requests_total{feature_flag_key,service_name}`
    —— flagd 自己的遥测。可以**经验性测出 flag→服务映射**（不用信描述文字），
    并作为注入生效校验的**一级判据**
  - 已定义**两级生效校验**：flagd 侧评估计数增长（必要）+ 下游症状偏移（充分）。
    两级都过才算场景有效

- [x] **生效校验器 + characterize 命令**（`internal/promq`、`internal/inject/verify.go`、
  `cmd/characterize`），一跑就抓到真问题（见下）
- [x] **🔴 重大纠错：catalog 有一半是错的** → `docs/faults.md` §2
  - 起因：烟测报 `cartFailure=100%` 为 inert（前后都是 0）。读源码发现它
    **只作用于 `EmptyCart`**，而实测 EmptyCart 只有 **0.03/s**
    （GetCart 0.67/s、AddItem 0.22/s），故障的**理论上限低于我设的 0.05/s 阈值**
    —— 失败得再彻底也读作 inert
  - 顺势 grep 了全部 13 个 flag 的源码作用点，纠正了 6 条：
    `imageSlowLoad` 其实是**浏览器组件设 Envoy 延迟头**（与 image-provider 无关，
    k6 不渲染页面所以大概打不到）；`recommendationCacheFailure` 其实是**内存泄漏**；
    `failedReadinessProbe` **从不碰请求路径**；`paymentUnreachable` 由 **checkout** 读；
    `kafkaQueueProblems` 根因是 **checkout** 不是 kafka
  - **结论：爆炸半径是「读 flag 那段代码」的属性，不是 flag 名字的属性。**
    每条 Spec 加了 `Site` 字段指向源码位置，有测试强制要求填
  - 代码相应改动：`MinDeltaOverride`（阈值高于故障上限会让它永远不可验证）、
    `SyntheticLoadReaches`（两条打不到的故障排除出场景库，有测试钉住）
- [x] **修了两个自己的 bug**
  - 一级判据写成「注入后评估计数有增长」是**错的**：load-generator 一直在跑，
    计数恒增，判据恒真。正确的是静态属性「有没有服务在读这个 flag」
  - `latencyP99` 的 fallback 链被**短路**：`(A or vector(0)) or B` 中第一项永远有值
    （A 为空就是 0），B 永远轮不到，只上报后面家族的服务会measure 出平坦的 0。
    默认值必须放链尾。这条是 `TestCatalogQueriesAreScalar`（对活 Prometheus 跑
    全部判据查询）抓出来的 —— **PromQL 出错是静默的**，标签拼错也会返回东西

- [x] **`queries/signals.yaml` — 信号查询的单一真源**（14 个信号，Go 侧
  `internal/signals` 已接入并测试通过；Python 工具层将读同一份）
  - 起因：Go 校验器和 Python 工具层都要「某服务的错误率/延迟」。各写一份的话，
    semconv 这种坑会在两个语言里各踩一次，而**第二个受害者是 Agent**
    —— 它会查到一个正在崩的服务、看到 0 错误、得出「服务健康」，
    它的能力等于被自己仪器的 bug 打了折
  - 阈值 `min_delta` 与查询放在一起，因为**阈值是单位的属性**
- [x] **🔴 又抓到一个严重 bug（假阳性）**：新 semconv 用
  `rpc_response_status_code="OK"`（字符串），不是 `rpc_grpc_status_code="0"`（数字）。
  而 **PromQL 里 `label!="0"` 会匹配「该标签不存在」的 series** ——
  所以我给 `checkout` / `product-catalog` 写的错误率查询把**所有请求都算成错误**。
  幸好在全量跑之前发现。有测试 `TestNewSemconvStatusLabels` 钉住四套状态约定
- [x] **改用错误比例而非绝对速率**。绝对阈值在这个 SUT 上系统性失效：
  故障的信号上限由「打到被破坏代码路径的流量」决定。实测各服务请求速率相差 100 倍
  （frontend 6.5/s ↔ checkout 0.067/s）。比例是尺度无关的
- [x] **单位纠错**（名字骗人）：`container_memory_percent_ratio` 实测返回 **93.76**，
  是 0..100 不是 0..1；`container_cpu_utilization_ratio` 实测 **1.586**，是核数不是比例。
  按「故障类别」定阈值对内存**差了 100 倍**
- [x] **发现三个服务完全没有服务端指标**：`payment` / `recommendation` / `email`
  实测 0.000 req/s。所以 `paymentFailure` 从 payment 自己的指标看**结构性不可见**。
  加了 `client_error_ratio` 走**调用方视角**（真实 SRE 判断「payment 是不是挂了」
  就是看它的调用方）。注意不能用 `server_address` 过滤 —— 实测那是**容器 IP**
  （`172.18.0.15`），重启就变；要用 `rpc_method=~"oteldemo.PaymentService/.*"`

- [x] **一级判据已重做** → `internal/dockerlog`
  - 改读 **flagd 容器日志**里的 `filepath event ... WRITE`。它对全部 13 个故障都成立，
    不依赖某个服务的 SDK 是否上报评估指标。写文件前先记下日志位置，
    避免被上个场景的事件满足
  - 诚实说：日志比指标弱（只证明 flagd 读了文件，不证明服务拿到了新值）。
    但它是唯一普适的信号，配合源码验证过的 `Site` 能覆盖它单独证明不了的部分
  - 新增 `UNREACHABLE` 判定类别，与"失败"区分。两个合成流量打不到的故障
    不再让整次运行非 0 退出 —— 否则一次正确的运行会永远看起来是坏的
- [x] **catalog 已接入 signals.yaml**，不再自己拼 PromQL。
  Go 校验器与 Python 工具层**由构造保证**测法一致，不靠纪律
- [x] **T5 只读诊断工具层完成** —— 12 个工具，22 个 Python 测试全过
  - 指标：`list_services` `get_service_health` `get_endpoint_breakdown`
    `get_client_calls` `get_resource_usage` `get_queue_lag` `promql`
  - 链路：`get_service_topology` `find_error_traces` `get_trace`
  - 日志：`query_logs` `find_error_logs`
  - **截断必须说明自己截断了**：Agent 分不清"看到全部"和"看到一部分"，
    就会得出数据不支持的结论
  - **工具失败返回可读结果而非抛异常**：诊断场景下失败是常态
    （被查的服务可能就是挂掉的那个），"这次调用失败因为 X"本身是观测
  - **拼错服务名必须与"该服务没有服务端指标"区分开** —— 两者都返回全 0，
    混在一起的话拼错会被当成证据。现在会报错并给近似匹配
  - `get_service_topology` **只用 Jaeger 实测调用数据**，不硬编码。
    硬编码等于把归因问题的一半答案直接给它，还会让「关掉拓扑工具」的消融失去意义。
    自环边已过滤（实测自环是调用量最大的 6 条边，留着会把真实依赖图埋掉）
  - `Registry.without()` 支持消融，一行调用即可撤掉某个工具
  - **实测验证**：characterization 注入 `adFailure` 期间，`find_error_traces`
    独立地报出了 `ad oteldemo.AdService/GetAds, 6 failed spans`；
    实测拓扑与读源码推出的依赖关系一致

### 🔄 进行中

- [ ] **T3 全量 characterization 正在跑**（13 个故障，约 52 分钟）
  - 第 1 个已出：`adFailure` **OK**，`before=0 → after=0.09524`（9.5% 错误比例）。
    改用比例判定后，这个原本被误判 inert 的故障通过了
  - 跑完后填 `docs/faults.md` §3 的「验证状态」列
  - ⚠️ **一级判据要重做**：flagd 的评估指标**只有 .NET 的 cart 服务上报**，
    烟测里 `adFailure` 因此被判 "dead"。除 cart 读的两个 flag 外，其余 11 个永远假阴性。
    替代方案：读 **flagd 容器日志**里的 `filepath event ... WRITE`（已确认它会打），
    这是通用的；评估指标降级为「有则作为补充证据」
  - 把 `catalog.go` 的内联查询换成 `signals.yaml`（同时自动获得漏掉的
    `rpc_server_call_duration_seconds` 家族）
  - 然后 `characterize` 全量跑 13 个故障（约 52 分钟），
  填 `docs/faults.md` §3 的「验证状态」列。未验证的故障不许进场景库
  - 已确认三套 semconv 并存，查询必须同时覆盖：
    `rpc_server_duration_milliseconds`（仅 ad）、
    `rpc_server_call_duration_seconds`（checkout / product-catalog）、
    `http_server_request_duration_seconds`（cart 等）、
    `http_server_duration_milliseconds`（frontend）
  - ⚠️ 待处理：`cart` 把 gRPC 记成 HTTP 且**状态码全是 200**（gRPC 状态在 trailer 里），
    错误率查询对 cart 可能天生看不见 —— 全量跑完确认

### ⏳ 待办（按依赖顺序）

| # | 任务 | 阻塞于 |
|---|---|---|
| T2 | 跑通 OTel Demo，验证 Prometheus / Jaeger / 日志可程序化访问 | Docker daemon |
| T3 | 摸清故障注入能力，产出故障清单表 | T2 |
| T4 | 故障注入器（Go）+ 场景库（含生效校验） | T3 |
| T5 | 只读诊断工具层（Python） | T2 |
| T6 | 评测 runner 与判分（Go）+ 随机 Agent 验证框架 | T4, T5 |
| T7 | 两条 baseline，拿到第一组数字 | T6 |
| T8 | 多步诊断 Agent（Python） | T6 |
| T9 | Go 动作执行层（Saga / 幂等 / 确认门 / 熔断 / 审计） | T8 |
| T10 | 消融实验与 Bad Case 归因 | T9 |

---

## ⛔ 当前阻塞

无硬阻塞。等镜像拉完。

待用户拍板的小事（不阻塞开发）：
- `git init` 已做，**首次 commit 未打**，等一句话。
- 若 Docker VM 7.75 GiB 频繁 OOM：要么在 Docker Desktop 上调内存，
  要么退到 `start-minimal`（去掉 kafka/accounting/fraud-detection，代价是丢掉
  `kafkaQueueProblems` 这条 L3 故障）。**优先建议上调内存**，别丢场景。

---

## 下一步（解除阻塞后立刻做）

```bash
make sut-fetch    # 拉 OTel Demo 到 sut/（gitignore，不入库）
make sut-up       # docker compose up -d
make sut-ps       # 等 15 个服务 healthy
cd gateway && go run ./cmd/probe
```

`probe` 全绿 = **整个项目最大的技术风险解除**（可观测面能被程序查询）。
它任何一项红，都要先解决再往下走，不要绕过。

之后立即进入 T3：摸 flagd 的 feature-flag 故障开关，产出故障清单表。
