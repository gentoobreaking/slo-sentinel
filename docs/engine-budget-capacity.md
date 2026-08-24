# 預算／容量引擎指南——判斷邏輯與完整資料流

> 本文說明 sentinel 最核心的部分：**一顆多視野 ETA 引擎，兩種感測家族**。
> 對應實作：`internal/budget`（引擎）、`internal/capacity`（容量感測）、
> `cmd/sentinel/daemon.go`（輪詢組裝）。演算法原始依據：`algs/capacity-eta.md`。
> **若本文與程式碼不一致，以程式碼為準**——請開 issue。

---

## 1. 一顆引擎、兩種感測

```
slo_defs/*.yaml          capacity_defs/*.yaml
（錯誤預算燃盡）            （磁碟/配額/連線數觸頂）
      │ sli_query               │ value + ceiling 查詢
      ▼                         ▼
┌─────────────────────────────────────────┐
│           budget 引擎（共用）              │
│  採樣校驗 → Theil–Sen 斜率 → 雙視野 ETA   │
│  → 狀態機判定 → 解除遲滯                  │
└─────────────────────────────────────────┘
      │ Forecast{state, utilization, eta×2}
      ▼
通知去重直推 Telegram ｜ 預測紀錄(/accuracy) ｜ /metrics ｜ /api/budget-status（CD 閘門）
```

兩個家族**只差輸入來源**；判定、狀態機、通知、API 全部共用同一套。

---

## 2. 輸入定義

### budget 家族——`slo_defs/*.yaml`

```yaml
slos:
  - id: api-availability
    service: api
    description: "API 可用性 99.9%"
    sli_query: 'sum(rate(http_requests_total{code=~"5.."}[5m])) / sum(rate(http_requests_total[5m]))'
    objective: 99.9        # SLO 目標百分比
    window_days: 28        # 計算視窗
```

天花板自動推導：**錯誤預算比 = (100 − objective) / 100**（99.9 → 0.001）。
消耗量 = sli_query 的即時值。利用率 = 消耗量 ÷ 預算比。

### capacity 家族——`capacity_defs/*.yaml`

```yaml
sensors:
  - id: data-disk
    desc: "資料磁碟"
    metric:
      value:   'node_filesystem_used_bytes{mount="/data"}'     # 消耗量 m(t)
      ceiling: 'node_filesystem_size_bytes{mount="/data"}'     # 天花板 C(t)，可為動態查詢
    horizons: [1h, 3d]           # 預設 [1h, 6h, 3d]
    thresholds:
      warn_eta: 48h              # 覆寫預設 72h
      crit_eta: 4h               # 覆寫預設 6h
      soft_ratio: 0.70           # 覆寫預設 0.80
```

> 注意：value/ceiling 必須是「保留原始數值」的 PromQL。
> 寫成 `used/size > 0.9` 這種布林會摧毀預測所需的序列資訊。

---

## 3. 每次輪詢的處理流程

```
InstantQuery(value) + RangeQuery(過往視窗)
        │
        ▼
① 採樣有效性校驗（不過→本輪不判定，記錄原因）
   ├─ 樣本覆蓋率 ≥ 視窗的 83%（MinSamples：視窗÷步長×0.83）
   └─ 天花板跳變 >1% → 清除斜率快取重算（擴容/縮容後舊斜率無效）
        │
        ▼
② Theil–Sen 穩健斜率
   └─ 所有樣本兩兩配對取斜率中位數——單次尖峰拉不動中位數
   └─ 跨越缺口的配對不納入（§A.5）
        │
        ▼
③ 雙視野 ETA 外插
   ├─ ETA_aggressive   = (C − m) ÷ 近窗斜率   ← 反映爆量情境
   └─ ETA_conservative = (C − m) ÷ 全窗斜率   ← 常態趨勢
   （β ≤ ε 或樣本不足 → 該視野 nil＝無法預測）
        │
        ▼
④ 狀態機判定（見 §4）
```

---

## 4. 判斷斷邏輯：狀態機的精確規則

| 狀態 | 進入條件（**任一命中即成立**） | 預設門檻 |
|---|---|---|
| 🔴 **critical** | ETA_aggressive < crit_eta | crit_eta = **6h** |
|                 | 或 利用率 ≥ crit_ratio       | crit_ratio = **0.95** |
| ⚠️ **warning**  | ETA_conservative < warn_eta | warn_eta = **72h** |
|                 | 或 ETA_aggressive < warn_eta **且** 利用率 ≥ soft_ratio | soft_ratio = **0.80** |
| ✅ **healthy**  | 以上皆非——但需**連續 2 輪**低於門檻才降級 | DefaultExitPolls = 2 |

判定優先序：critical ＞ warning ＞ healthy。

> **雙視野並陳的理由**（拒絕單窗）：只用近期斜率會被一次性尖峰嚇到；
> 只用全窗斜率會對「剛上線的新服務」反應遲鈍。激進視野抓風險、
> 穩健視野避免誤報——通知時兩者並列，讓人自己判讀。
>
> **解除遲滯的理由**：指標在門檻邊緣抖動時，沒有遲滯會產生
> critical→healthy→critical 的通知轟炸。連續 2 輪確認才降級。

---

## 5. 判定之後：下游功能鏈

| 功能 | 行為 | 對應 |
|---|---|---|
| **直推 Telegram** | 狀態轉移才推（同狀態不重複）；人話卡含雙視野 ETA | `formatForecastCard` |
| **F2b 協調靜默** | AlertManager 已有 firing 的對應告警 → sentinel 不重複推 | `amcoord.HasFiringAlerts` |
| **預測紀錄** | 每次 AppendPrediction → `/api/accuracy` 自評命中率 | T009/T018 |
| **Prometheus metrics** | `sentinel_eta_seconds{sensor,horizon}`、`sentinel_capacity_used_ratio` | 僅觀測，不作告警輸入 |
| **CD 閘門端點** | `/api/budget-status/{id}` → CI step 查詢 | F6 Phase 1 |
| **每日摘要** | 彙整當日所有狀態變化 | digest |

### 通知卡長相（真實格式）

```
⚠️ data-disk
使用率 87.3%｜餘量 452GB
若持續爆量：約 18.2 小時後觸頂
回到常態：尚餘 4.7 天
```

---

## 6. 門檻調整速查

| 想調整什麼 | 改哪裡 | 預設 |
|---|---|---|
| warning/critical 的 ETA 門檻 | def 檔 `thresholds.warn_eta / crit_eta` | 72h / 6h |
| 提前警告的水位 | `thresholds.soft_ratio` | 0.80 |
| critical 水位 | `thresholds.crit_ratio` | 0.95 |
| 預測視野 | `horizons` | [1h, 6h, 3d] |
| 輪詢間隔 | config `poll_interval_sec` | 60s |

門檻一律**人工調整**（系統不會自己漂移）；校準依據來自
`/accuracy` 的命中統計與 ≥30 天運行數據（T019/T021 的前置條件）。

capacity 與 budget（SLO）兩家族皆支援 per-sensor 門檻覆寫：capacity 寫在
`capacity_defs/*.yaml` 的 `thresholds` 區塊；SLO 寫在 `slo_defs/*.yaml` 的
`thresholds` 區塊（四欄皆可選，未寫用預設；非法組合啟動即報錯）：

```yaml
slos:
  - id: api-availability
    # ...
    thresholds:
      warn_eta: 48h      # 可選；以下四者未寫用預設 72h / 6h / 0.80 / 0.95
      crit_eta: 4h
      soft_ratio: 0.70
      crit_ratio: 0.90
```

> 歷史備註：budget 家族曾鎖定預設值不可調（T023 已實作解除）。

---

## 7. 與 rules.d 的關係

- rules.d 中標成 `budget`/`capacity` 的條目**只是目錄登記**（供 amcoord 比對），
  sentinel 不執行它們的 expr——因為布林 expr 裝不下預測所需的原始序列
- 真正的感測輸入永遠走 `slo_defs/` 與 `capacity_defs/`
- 完整分類學見 [`docs/rule-classification.md`](rule-classification.md)

---

## 8. 本機驗證方式

```bash
docker compose --profile dev up -d --build
# 等 ~1 分鐘後：
curl -s http://127.0.0.1:9099/api/status.json | python3 -m json.tool
# → dev-root-disk 應出現 state=healthy、last_value=真實磁碟用量
docker logs slo-sentinel | grep capacity_polled
# → capacity_polled sensor=dev-root-disk state=... utilization=...
```
