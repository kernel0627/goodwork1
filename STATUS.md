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

### 🔄 进行中

- [ ] **T3 收尾**：逐条注入 13 个故障，实测可观测签名，填 `docs/faults.md` 的「验证状态」列

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
