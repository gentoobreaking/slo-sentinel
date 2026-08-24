# 規則分類與使用指南——sentinel_kind 判定邏輯全解

> 本文是 `rules.d/` 感測目錄的分類與使用完整參考。
> 對應實作：`internal/catalog`（載入/分類）、`internal/catalog/autoclassify.go`
> （自動分類）、`internal/waste/waste.go`（waste 掃描執行）。
> **若本文與程式碼不一致，以程式碼為準**——請開 issue。

---

## 1. 大局：兩層監控架構

sentinel 不取代 Prometheus/AlertManager，而是與它**平行互補**：

```
                        ┌─ 前瞻層：sentinel ──── slo_defs/*.yaml（SLO 預算燃盡預測）
你的服務指標 ──→ Prometheus ┤                         capacity_defs/*.yaml（容量觸頂預測）
                        │                         rules.d（waste 家族掃描）
                        │
                        └─ 反應層：AlertManager ── rules.d 中「給 AM 的」靜態閾值規則
                            （越線就響；sentinel 只查詢它做 F2b 靜默協調）
```

| 問題型態 | 該用哪層 | 例子 |
|---|---|---|
| 「現在已經越線了」 | AlertManager | redis 掛掉、磁碟 >90%、憑證快過期 |
| 「照目前速度，之後會燒穿」 | sentinel | 錯誤預算 16h 後耗盡、磁碟三天後滿 |
| 「長期沒在用，是不是該刪」 | sentinel（waste） | 孤兒 PVC、殭屍主機、零流量 ELB |

同一份 YAML 放進 Promtheus 或放進 sentinel，行為完全不同（見 §4）。

---

## 2. 四種分類：判定邏輯

每條規則載入時，`catalog.Classify()` 依以下**優先順序**判定（先命中先停）：

```
① labels["sentinel_kind"] 有明確值？
   ├─ 值 = budget / capacity / waste → 直接採用
   └─ 值 = 其他任何字串              → KindNone（打錯字不會變成誤動作）

② 任一 label 的 key 以 "sloth" 開頭？
   └─ 是 → KindBudget（Sloth 生成的 SLO 規則慣例）

③ record 名稱以 "sentinel_" 開頭？
   └─ 是 → KindCapacity（正規化系列慣例）

④ annotations["sentinel_sensor"] 存在？
   └─ 是 → KindCapacity

⑤ 都不是 → KindNone（載入但不路由：供 amcoord 比對與目錄完整性）
```

> **設計原則：預設安全。** 未標籤或標錯值的規則一律躺平（KindNone），
> 最壞結果是「這條沒生效」，不是誤報或誤執行。

### 各 kind 是什麼

| kind | 意義 | 誰執行它 | 執行後產出 |
|---|---|---|---|
| `budget` | SLO 錯誤預算相關規則 | ❌ sentinel 引擎不執行此類 expr——預算感測來自 `slo_defs/*.yaml` 的 `sli_query` | 目錄條目（amcoord 比對用） |
| `capacity` | 容量觸頂相關規則 | ❌ 同上——容量感測來自 `capacity_defs/*.yaml` | 目錄條目 |
| `waste` | 瘦身影候選偵測 | ✅ **sentinel 直接執行 expr**（見 §3） | Candidate → Telegram 候選通知＋生命週期追蹤 |
| `KindNone`（無標籤） | 非 sentinel 管理 | ❌ | 目錄條目 |

> ⚠️ 誠實說明：rules.d 裡的 budget/capacity 分類條目**只是目錄登記**，
> 真正的預算/容量感測輸入永遠來自 `slo_defs/` 與 `capacity_defs/`。
> 只有 **waste 家族是從 rules.d 直接執行**的。

---

## 3. waste 家族的執行流程（唯一被 sentinel 執行的家族）

```
rules.d 載入（遞迴掃 *.y*ml，promtool 驗證，失敗整檔隔離）
    │  Classify → KindWaste 的 alert 規則
    ▼
waste.Scanner.Scan（每次 /api/waste 觸發或排程掃描時）
    │  對每條規則：
    │  1. 取 for 為回看視窗（未設定 → 預設 14 天）
    │  2. RangeQuery(expr, now−window, now, 步長 24h)
    │  3. 視窗內【所有樣本】≥ 0.5 → 候選成立
    ▼
Candidate{
    SensorID = labels.sentinel_sensor ＞ annotations.sentinel_sensor
               ＞ record 名 ＞ alert 名
    Reason   = annotations.summary      ← 告警訊息只吃 summary，不吃 description
    Renotify = labels.notify_every      ← 重提週期（未設定用 tracker 預設）
}
    ▼
Tracker 生命週期：
    observe → 通知（同資源去重）→ dismiss（暫緩至期限）→ resolve（累積節省金額）
```

> **`for` 在這裡的語意**：不是 Prometheus 的「pending 持續時間」，
> 而是「回看窗」——expr 必須在整個視窗內持續成立才算候選。
> 所以 waste 規則的 `for: 14d` 意思是「已經連續閒置 14 天」。

---

## 4. 同一份規則放 Prometheus vs 放 sentinel 的差異

| 面向 | Prometheus rule_files | sentinel rules.d |
|---|---|---|
| 執行者 | Prometheus 即時評估（scrape 週期粒度） | sentinel Scanner 回顧式掃描（24h 步長） |
| `for` 語意 | pending → firing 的未來式門檻 | 回看窗內全真才成立的回顧式認定 |
| 觸發時效 | 分鐘級 | 天級（慢性問題） |
| 通知 | AM 路由 → receiver | Telegram 人話卡＋Tracker 生命週期 |
| 前提 | prometheus.yml 設 `rule_files` | 規則要有 `sentinel_kind: waste`（否則躺平） |

> **每條規則選一個家**：放兩邊＝同一件事吵兩次（F2b 靜默只涵蓋
> capacity/SLO 感測輪詢迴圈，不含 waste 掃描）。

---

## 5. 自動分類：不用自己判斷 kind

社群規則經 `scripts/sync-community.sh` 接入時，`cmd/ruleclassify` 會依
**內容啟發式**（`catalog.SuggestKind`）自動補上 `sentinel_kind`。
使用者只需挑服務（`SELECTED` 清單），不必懂分類學。

### 判斷信號（依序評估，先命中先停）

| 順位 | 信號 | 建議 kind | 例子 |
|---|---|---|---|
| 1 | expr 引用 `sloth[:_]` 指標，或任一 label key 有 `sloth_` 前綴 | budget | `sloth:sli_error:ratio_rate5m > x` |
| 2 | annotations（summary/description）含 idle/unused/orphan/zombie/stale/unattached/no_requests… | waste | "volume unused for weeks" |
| 3 | expr 含 `*_over_time[...Nd]`（天級回看窗） | waste | `avg_over_time(x[30d]) < 1` |
| 4 | expr（剝除引號後）含 idle/orphan/stale/unattached | waste | `disk_io_idle_days > 30` |
| 5 | `for` ≥ 7 天 | waste | `for: 1w` |
| 6 | 都沒命中 | **不加標籤**（KindNone） | 一般閾值告警 |

> **為什麼要剝除引號**：`rate(node_cpu_seconds_total{mode="idle"}[5m])`
> 裡的 `idle` 是 CPU 閒置時間的技術參數，不是浪費語意——
> 比對前先移除所有 `"..."` 字串，避免誤判。

### 安全設計

- 已有 `sentinel_kind` 的規則**一律不覆寫**（人工判斷永遠贏過啟發式）
- 用 yaml.Node 原位改寫：保留上游檔案的註解與格式
- 分類錯的最壞情況：「多一條被當候選掃」或「少一條沒掃」——git diff 一眼可見

---

## 6. 三條典型使用路徑

### 路徑 A：Sloth 生成 SLO rules（反應層備援，選配）

```
slo_defs/api.yaml ──sloth generate──→ rules.d/sloth-generated/api.yaml
                                            │ labels 含 sloth_* → 自動歸 budget
sentinel 同時直接讀 slo_defs 做前瞻預測（不需要這些 rules 也能跑）
```

### 路徑 B：社群規則挑選同步（本檔 §5）

```
echo "redis" >> rules.d/community/SELECTED
./scripts/sync-community.sh     # 拉 → 複製 → ruleclassify 自動補標籤
```

上游完整快取在 `.community-upstream/`（gitignore，位於載入路徑之外）；
只有 SELECTED 的服務會進入 `rules.d/community/`。

### 路徑 C：手寫感測定義（最常用）

```
slo_defs/*.yaml       → SLO 預算燃盡感測（不需要任何 rules.d 條目）
capacity_defs/*.yaml  → 容量觸頂感測（同上）
rules.d/k8s-waste.yaml ← 內建 K8sProvider/StandaloneProvider 產出（已預分類）
```

---

## 7. 常用 labels／annotations 速查

| key | 用途 | 適用 |
|---|---|---|
| `sentinel_kind` | 明確指定分類（budget/capacity/waste） | 所有規則；人工覆寫優先 |
| `sentinel_sensor` | 候選/感測 id（覆寫 record/alert 名） | waste、capacity |
| `notify_every` | waste 候選重提週期（Prometheus duration） | waste |
| `sentinel_exclude_namespaces` | 排除命名空間（逗號分隔，支援 `openshift-*` 萬用字尾） | waste（K8s） |
| `annotations.summary` | 候選通知的人話理由 | waste |
| labels key 前綴 `sloth_` | Sloth 慣例 → 自動歸 budget | Sloth 生成 |

---

## 8. 決策速查：一條新規則該放哪？

```
這條規則觸發時，你需要的是——
│
├─「現在立刻處理」（服務掛了、資源耗盡中）
│    → 放 Prometheus（rule_files）＋ AlertManager
│
├─「列入觀察清單，慢慢確認後清理」（閒置、孤兒、低利用率）
│    → 加 labels: { sentinel_kind: waste, sentinel_sensor: <id> }
│      放 rules.d，交給 sentinel 瘦身影生命週期
│
└─「它是預算消耗速率的 SLI」
     → 寫進 slo_defs/*.yaml 的 sli_query（不是 rules！）
```
