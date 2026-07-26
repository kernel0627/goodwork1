# 被测系统（SUT）事实清单

> 对象：`open-telemetry/opentelemetry-demo`
> 版本：`1755859`（2026-07-24，release commit `#3761`）
> 位置：`sut/opentelemetry-demo/`（**已 gitignore，不入库**，用 `make sut-fetch` 拉取）
>
> 本文件记录**实地勘察到的事实**，不是从文档抄的。上游改版后需要重新核对。

---

## 1. Compose 布局

上游已从单个 `docker-compose.yml` 改为 `compose.yaml` + overlay：

| 文件 | 内容 |
|---|---|
| `compose.yaml` | 核心 20 项：`ad cart checkout currency email frontend frontend-proxy image-provider load-generator payment product-catalog quote recommendation shipping flagd flagd-ui telemetry-docs astronomy-db valkey-cart otel-collector` |
| `compose.observability.yaml` | `jaeger grafana prometheus opensearch opamp-server`（并覆写 `frontend-proxy` `otel-collector`） |
| `compose.full.yaml` | `accounting fraud-detection kafka`（并覆写 `checkout` `otel-collector`） |
| `compose.extras.yaml` | 杂项覆写 |
| `compose.agent.yaml` | `agent mcp chatbot` — **上游自带的 AI 助手，与本项目无关，不要启用**（会污染观测面并混淆归因） |
| `compose.profiling.yaml` / `compose.tests.yaml` | 不用 |

上游 `make start` 实际展开为：

```
docker compose --env-file .env --env-file .env.override \
  -f compose.yaml -f compose.full.yaml -f compose.observability.yaml -f compose.extras.yaml \
  up --force-recreate --remove-orphans --detach
```

> ⚠️ 两个 `--env-file` 缺一不可。直接在该目录 `docker compose up` 会**只加载 `compose.yaml`
> 且缺环境变量**，起不来观测面。本项目 Makefile 的 `sut-up` 已按上述完整形式实现。

---

## 2. 网络暴露 —— 这里有个坑

**几乎所有服务都没有 host 端口映射。** compose 里写的是 `- "${X_PORT}"`（单值形式），
只是 expose，不发布到宿主机。对外只有 **frontend-proxy（Envoy）的 8080**。

Envoy 路由前缀（实地从 `src/frontend-proxy/*.yaml` 读出）：

| 前缀 | 去向 |
|---|---|
| `/` | frontend（catch-all，必须放最后） |
| `/jaeger/` | jaeger |
| `/grafana/` | grafana |
| `/feature` | flagd-ui |
| `/flagservice/` | flagd（OFREP） |
| `/telemetry/` | telemetry-docs |
| `/images/` | image-provider |
| `/otlp-http/` `/opamp/` `/profiles/` `/chatbot/` | 其余 |

### ⚠️ Prometheus 不可从宿主机访问

`prometheus` 服务**既没有 ports 映射，也没有 Envoy 路由**。它只在 compose 网络内
以 `prometheus:9090` 存在（`.env`：`PROMETHEUS_PORT=9090` `PROMETHEUS_HOST=prometheus`）。

这直接挡住诊断工具层——Agent 必须能跑 PromQL。

**解法**：本项目在自己的目录下提供 overlay `sut/compose.medic.yaml`，
只做一件事：把 Prometheus 的 9090 发布到宿主机。**不修改 SUT 仓库任何文件。**

### 实测可用地址（`make probe` 5/5 通过）

| 用途 | 地址 | 备注 |
|---|---|---|
| Demo 前端 | `http://localhost:8080` | |
| **Jaeger API** | `http://localhost:8080/jaeger/ui/api/...` | ⚠️ base path 是 **`/jaeger/ui`** 不是 `/jaeger`。`/jaeger/` 和 `/jaeger/api/...` 都 404；`/jaeger/ui/api/services` 与 `/jaeger/ui/api/v3/services` 均 200 |
| Grafana | `http://localhost:8080/grafana/` | |
| flagd UI | `http://localhost:8080/feature/` | |
| **Prometheus** | `http://localhost:9090` | **依赖 `compose.medic.yaml` overlay** |

### ⚠️ Prometheus 没有 scrape target

`prometheus` 以 `--web.enable-otlp-receiver` 启动，指标由 otel-collector **推送**进来，
**scrape target 恒为 0/0**。

> 因此 `/api/v1/targets` 不能用作健康判据，也不能用来枚举服务。
> 正确做法是查 `/api/v1/label/service_name/values`。
> 探针 `cmd/probe` 已按此实现（实测 22 个 service_name；Jaeger 侧 19 个 service）。

这条对 T5 的工具层同样成立：`get_service_topology` 与服务枚举都要走 `service_name` 标签
和 Jaeger 的实际调用数据，不要指望 scrape 元数据。

---

## 3. 故障注入通道 —— flagd 的 bind mount

`flagd` 服务定义（`compose.yaml`）：

```yaml
flagd:
  command: ["start", "--uri", "file:./etc/flagd/demo.flagd.json"]
  volumes:
    - ./src/flagd:/etc/flagd
```

配置来自 **bind mount 的文件**，flagd 以 `file:` URI 监听它。

> **结论：改宿主机上的 `sut/opentelemetry-demo/src/flagd/demo.flagd.json`，
> 就等于注入 / 撤销故障。** 不需要 API、不需要 exec 进容器、不需要重启服务。
>
> 这是本项目故障注入器的实现方式：**原子写文件**（写临时文件后 rename）。

注入器仍必须做**生效校验**（见 `docs/design.md` §7）：写完文件后要确认故障
真的在指标上出现，否则该场景作废。flagd 的热加载不是即时的，也不保证成功。

故障清单见 [`docs/faults.md`](./faults.md)。

---

## 4. 资源与服务精简

宿主机 24 GB / 10 核 / 244 GB 可用。

Docker VM 分到 **7.75 GiB / 10 CPU**，28 个容器实测占 **~4.3 GB**，余量约 3.4 GB，够用。

> ⚠️ **不要试图改 `settings-store.json` 来调 VM 内存。** 实测：写入 `MemoryMiB` / `Cpus`
> 之后 Docker Desktop 会**自行剥掉这两个 key**（不认这个 schema），而且那一次重启
> VM 起不来（backend log 报 `no route to host` 到 `192.168.65.7:2376`），
> 必须完全退出 Docker 再重启才恢复。要调内存只能走 GUI 的 Settings → Resources。

### 实测内存占用（全量 28 容器）

| 容器 | 占用 | 容器 | 占用 |
|---|---:|---|---:|
| opensearch | 838 MB | grafana | 164 MB |
| kafka | 562 MB | flagd-ui | 164 MB |
| load-generator | 390 MB | accounting | 143 MB |
| jaeger | 340 MB | cart | 111 MB |
| otel-collector | 315 MB | 其余 19 个 | < 70 MB 各 |
| ad | 223 MB | | |
| fraud-detection | 222 MB | **合计** | **~4.3 GB** |
| prometheus | 170 MB | | |

### 28 个容器不可裁剪 —— 已验证

一开始我以为能砍掉纯 UI 的 `flagd-ui`(164 MB)、`telemetry-docs`(10 MB)、
`grafana`(164 MB) 和只被日志管道用到的 `opensearch`(838 MB)。**不行。**

用 `docker compose config`（合并后的权威视图，不是逐个文件猜）实测：

```
frontend-proxy  depends_on -> flagd-ui  grafana  telemetry-docs  jaeger  opamp-server  frontend
otel-collector  depends_on -> opensearch  jaeger  opamp-server
```

`frontend-proxy` 是**唯一的宿主机入口**（8080，所有观测面都从这儿进），
它把 flagd-ui / grafana / telemetry-docs 一起拽起来；
`otel-collector` 是遥测管道核心，它拽起 opensearch。
**即使 `up` 时只列想要的服务，compose 也会按 depends_on 全部补齐**（实测确认）。

> **踩过的坑**：手写脚本逐个解析 compose 文件时，我让后面的文件**覆盖**了
> `depends_on`，而 compose 实际是**合并**。于是漏掉了 `compose.yaml` 里
> frontend-proxy 对 flagd-ui / telemetry-docs 的依赖，得出了错误结论。
> **依赖关系一律用 `docker compose config` 取，不要自己解析 YAML。**
> `make sut-deps` 就是干这个的。

唯一能砍的办法是用 `!override` 改写这两个服务的 `depends_on` ——
那就是**修改被测系统本身**。

> **底线：被测系统一旦被改，评测结果失去可比性。**
> 省 1 GB 内存换不来一句「你这个 demo 是不是被你改坏了」。
> 4.3 GB / 7.75 GiB 有 3.4 GB 余量，够用。

`compose.medic.yaml` 之所以只发端口、不动别的，也是这条底线。

### 内存限制反而对本项目有利

各服务 compose 里都设了 `deploy.resources.limits.memory`（flagd 75M、prometheus 200M 等）。
这意味着内存类故障（`emailMemoryLeak`）会**真的触发容器限制**，
不是纸面故障 —— 会看到真实的 OOM / 重启行为。
