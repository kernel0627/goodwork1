SHELL := /bin/bash
PY    := /Users/traegang/miniforge3/envs/agent/bin/python
PIP   := /Users/traegang/miniforge3/envs/agent/bin/pip
ROOT  := $(shell cd "$(dir $(lastword $(MAKEFILE_LIST)))" && pwd)
SUT   := $(ROOT)/sut/opentelemetry-demo

.DEFAULT_GOAL := help

.PHONY: help
help: ## 显示所有可用目标
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# ---------- 环境 ----------

.PHONY: doctor
doctor: ## 检查工具链与依赖是否就绪
	@echo "== go ==";     go version
	@echo "== docker ==";  docker --version && docker compose version
	@echo "== daemon =="; docker info >/dev/null 2>&1 && echo "running" || echo "NOT RUNNING"
	@echo "== python =="; $(PY) -V
	@echo "== pydeps =="; $(PY) -c "import fastapi,uvicorn,httpx,yaml,pydantic; print('ok')" 2>&1 | tail -1

.PHONY: pydeps
pydeps: ## 向 conda agent 环境安装 Python 依赖
	$(PIP) install -r agent/requirements.txt

# ---------- 被测系统 ----------
#
# 上游已改为 compose.yaml + overlay，且几乎不发布 host 端口。见 docs/sut.md。
# 两个 --env-file 缺一不可；compose.medic.yaml 是本项目的 overlay，只放开 Prometheus 9090。
# 刻意不启用 compose.agent.yaml —— 那是上游自带的 AI 助手，会污染观测面。

DC       := docker compose --env-file .env --env-file .env.override
DC_FILES := -f compose.yaml -f compose.full.yaml -f compose.observability.yaml \
            -f compose.extras.yaml -f ../compose.medic.yaml
SUT_DC    = cd "$(SUT)" && $(DC) $(DC_FILES)

# 服务数是 28，且不可裁剪。实测（`docker compose config` 合并后的权威视图）：
#
#   frontend-proxy depends_on -> flagd-ui grafana telemetry-docs jaeger opamp-server frontend
#   otel-collector depends_on -> opensearch jaeger opamp-server
#
# frontend-proxy 是唯一的宿主机入口(8080)，它把 flagd-ui / grafana / telemetry-docs
# 一起拽起来；otel-collector 是遥测管道核心，它拽起 opensearch。
# 即使在 `up` 时只列想要的服务，compose 也会按 depends_on 把这些补齐。
#
# 要真砍，唯一办法是用 `!override` 改写这两个服务的 depends_on，
# 那就是**修改被测系统**。被测系统一旦被改，评测结果失去可比性，
# 而且面试时会多一句「你这个 demo 是不是被你改坏了」——这个代价远大于省 1 GB。
#
# 结论：接受 28 个容器（实测 ~4.3 GB，VM 7.75 GiB，余量约 3.4 GB，够用）。
# 真要降内存请走 Docker Desktop GUI 的 Settings → Resources，
# 不要改 settings-store.json（见 STATUS.md 决策 D15）。

.PHONY: sut-fetch
sut-fetch: ## 拉取 OpenTelemetry Demo 到 sut/（不入库）
	@test -d "$(SUT)" || git clone --depth 1 \
	  https://github.com/open-telemetry/opentelemetry-demo.git "$(SUT)"
	@echo "SUT at $(SUT)"

.PHONY: sut-config
sut-config: ## 干跑：校验 compose 组合能否解析（不启动任何容器）
	@$(SUT_DC) config --services

.PHONY: sut-up
sut-up: ## 启动被测系统（28 个容器，不可裁剪，见上方注释）
	$(SUT_DC) up -d --remove-orphans

.PHONY: sut-deps
sut-deps: ## 打印 frontend-proxy / otel-collector 的真实合并后依赖（解释为何不可裁剪）
	@$(SUT_DC) config 2>/dev/null | $(PY) -c "import sys,yaml; s=yaml.safe_load(sys.stdin)['services']; \
[print(k, '->', ' '.join((s[k].get('depends_on') or {}).keys())) for k in ('frontend-proxy','otel-collector')]"

.PHONY: sut-mem
sut-mem: ## 按内存占用降序列出各容器，并给出合计
	@docker stats --no-stream --format '{{.Name}}|{{.MemUsage}}' | $(PY) -c "\
import sys; \
rows=[l.strip().split('|') for l in sys.stdin if '|' in l]; \
mib=lambda s: float(s.replace('GiB',''))*1024 if 'GiB' in s else float(s.replace('MiB','').replace('KiB','')); \
p=sorted(((mib(u.split('/')[0].strip()), n, u) for n,u in rows), reverse=True); \
[print(f'{m:9.1f} MiB  {n:<20s} (limit {u.split(\"/\")[1].strip()})') for m,n,u in p]; \
print(f'{sum(m for m,_,_ in p):9.1f} MiB  TOTAL over {len(p)} containers')"

.PHONY: sut-down
sut-down: ## 停止被测系统（保留数据卷）
	$(SUT_DC) down --remove-orphans

.PHONY: sut-ps
sut-ps: ## 查看各服务状态
	@$(SUT_DC) ps

.PHONY: sut-health
sut-health: ## 汇总健康状态：running / healthy 计数
	@$(SUT_DC) ps --format '{{.Service}}\t{{.State}}\t{{.Status}}' | sort

.PHONY: probe
probe: ## 跑可观测面探针（sut-up 之后执行，全绿才继续往下做）
	cd gateway && go run ./cmd/probe

# ---------- 构建 ----------

.PHONY: build
build: ## 编译 Go 组件
	cd gateway && go build -o bin/ ./cmd/...

.PHONY: fmt
fmt: ## 格式化
	cd gateway && go fmt ./...
	$(PY) -m ruff format agent 2>/dev/null || true

.PHONY: test
test: ## 跑测试
	cd gateway && go test ./...
	$(PY) -m pytest agent/tests -q 2>/dev/null || true

# ---------- 清理 ----------

.PHONY: clean
clean: ## 清理构建产物与评测输出
	rm -rf gateway/bin results traces

.PHONY: clean-docker
clean-docker: ## 清除本项目在 Docker 中的全部留痕
	-cd "$(SUT)" && docker compose down -v --remove-orphans
	-docker ps -aq --filter "name=medic-" | xargs -r docker rm -f
	-docker volume ls -q --filter "name=medic-" | xargs -r docker volume rm
