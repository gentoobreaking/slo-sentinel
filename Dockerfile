# slo-sentinel 容器化建置
#
# 多階段建置：
#   1. builder：golang:alpine（latest）靜態編譯——modernc.org/sqlite 為純 Go，CGO_ENABLED=0
#   2. runtime：alpine:latest（latest）——僅複製 binaries＋promtool（選配 rules 驗證）
#
# 兩個進入點共用同一映像：
#   docker run ... slo-sentinel:latest            # daemon（預設 CMD）
#   docker run ... slo-sentinel:latest sentinel-ui # 唯讀 Web UI

# ---- build stage ----
FROM golang:alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
COPY config/ config/

RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags "-s -w" \
      -o /out/sentinel ./cmd/sentinel \
 && CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags "-s -w" \
      -o /out/sentinel-ui ./cmd/sentinel-ui

# ---- runtime stage ----
FROM alpine:latest

# promtool：rules.d 語法驗證（選配功能；缺了只是降級略過）
# su-exec：entrypoint 修正資料卷權限後降權為 sentinel 用戶
RUN apk add --no-cache prometheus su-exec && \
    addgroup -S sentinel && adduser -S -G sentinel sentinel && \
    mkdir -p /etc/sentinel /var/lib/sentinel /var/lib/sentinel/pricing-cache /srv/sentinel && \
    chown -R sentinel:sentinel /var/lib/sentinel

COPY --from=builder /out/sentinel /usr/local/bin/sentinel
COPY --from=builder /out/sentinel-ui /usr/local/bin/sentinel-ui
COPY deploy/docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# 預設掛載點（唯讀設定）
#   /etc/sentinel/           sentinel.yaml、cost_map.yaml、sentinel-ui.json
#   /srv/sentinel/rules.d    感測目錄（熱載入）
#   /srv/sentinel/slo_defs   SLO 定義
#   /srv/sentinel/capacity_defs  容量感測定義
#   /var/lib/sentinel        SQLite（WAL）＋pricing 快取（需可寫）
VOLUME ["/var/lib/sentinel"]

USER root
WORKDIR /var/lib/sentinel

# 9099 唯讀 JSON API｜9102 Prometheus metrics｜9098 Web UI
EXPOSE 9099 9102 9098

# shell form：exec form 不支援 ||；wget 由 alpine busybox 提供
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD wget -qO- http://127.0.0.1:9099/api/status.json || exit 1

# 預設跑 daemon；UI 以 `command: sentinel-ui` 覆寫（見 docker-compose.yml）
ENTRYPOINT ["/entrypoint.sh"]
CMD ["/usr/local/bin/sentinel", "-config", "/etc/sentinel/sentinel.yaml"]
