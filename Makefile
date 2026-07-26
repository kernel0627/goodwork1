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

.PHONY: sut-fetch
sut-fetch: ## 拉取 OpenTelemetry Demo 到 sut/（不入库）
	@test -d "$(SUT)" || git clone --depth 1 \
	  https://github.com/open-telemetry/opentelemetry-demo.git "$(SUT)"
	@echo "SUT at $(SUT)"

.PHONY: sut-config
sut-config: ## 干跑：校验 compose 组合能否解析（不启动任何容器）
	@$(SUT_DC) config --services

.PHONY: sut-up
sut-up: ## 启动被测系统（首次会拉十几个镜像，耗时较长）
	$(SUT_DC) up -d --remove-orphans

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
