# slo-sentinel 部署指南（T017）

> 前瞻預測層：SLO 預算燃盡 + 容量觸頂 + 成本推估 + 瘦身閒置偵測
> 依賴：Prometheus（指標源）、Sloth（SLO 規則生成，選配但建議）、AlertManager（靜態告警）

## 元件

| Binary | 角色 | 監聽 |
|---|---|---|
| `bin/sentinel` | daemon：輪詢/預測/直推通知；同時提供唯讀 JSON API 與 /metrics | API 127.0.0.1:9099、metrics 127.0.0.1:9102 |
| `bin/sentinel-ui` | 唯讀網頁總表 | 127.0.0.1:9098 |

**直推中心定案**：通知一律由 sentinel 直推 Telegram 人話卡；
`/metrics` 僅供 Grafana 觀測，不作為告警規則的輸入（避免雙路通知）。

## 建置

```bash
make build    # 產出 bin/sentinel bin/sentinel-ui
make test     # 全套單元測試
```

## 設定

```bash
cp docs/sentinel.yaml.example sentinel.yaml   # 再編輯
```

| 鍵 | 預設 | 說明 |
|---|---|---|
| poll_interval_sec | 60 | 輪詢間隔 |
| prometheus_url | http://localhost:9090 | 指標源 |
| alertmanager_url | http://localhost:9093 | F2b 協調靜默查詢用 |
| telegram_token / TELEGRAM_CHAT_ID | — | 直推通知；未設定時降級 log-only |
| listen_addr | 127.0.0.1:9099 | 唯讀 API（UI 資料源）——**勿對外公開** |
| metrics_addr | 127.0.0.1:9102 | Prometheus scrape 目標 |

## rules.d 佈建（感測目錄）

1. **SLO 家族**：以 Sloth 從 `slo_defs/*.yaml` 生成規則：
   ```bash
   sloth generate -i slo_defs/api.yaml -o rules.d/sloth-generated/api.yaml
   ```
2. **community/**：從 awesome-prometheus-alerts 最新版取需要的規則檔，
   並將 commit hash 寫入 `rules.d/community/UPSTREAM_COMMIT`
3. **capacity_defs/*.yaml**：容量感測定義（value/ceiling 兩條 PromQL + 門檻）
4. 啟動前驗證：`promtool check rules rules.d/**/*.yaml`

sentinel 的 `/metrics` 暴露 `sentinel_eta_*` 等觀測指標——請在 Prometheus 加入
scrape job（僅供 Grafana 觀測與自我監控，**非告警輸入**）。

## systemd

```ini
# /etc/systemd/system/sentinel.service
[Unit]
Description=slo-sentinel daemon
After=network-online.target

[Service]
ExecStart=/opt/slo-sentinel/bin/sentinel -config /etc/slo-sentinel/sentinel.yaml
Environment=TELEGRAM_CHAT_ID=-1001234567890
Restart=on-failure
# graceful shutdown：完成當前輪詢後退出

[Install]
WantedBy=multi-user.target
```

## 安全邊界摘要

- 所有監聽預設綁 127.0.0.1；對外一律經反向代理認證（Tailscale/Caddy forward_auth）
- Telegram token 未設定 → 降級 log-only，不會半套運作
- UI 無寫入端點（僅 GET），最壞風險為唯讀資料暴露
