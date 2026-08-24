# 定義手冊——capacity_defs／slo_defs／rules.d 完整撰寫參考

> 本文件是三個定義目錄的**單一權威參考**（single source of truth）。
> 欄位語意以程式碼為準：`internal/capacity/defs.go`、`internal/spec/spec.go`、
> `internal/catalog/`；原理深究請讀 [engine-budget-capacity.md](engine-budget-capacity.md)
> 與 [rule-classification.md](rule-classification.md)。

---

## 1. 三分鐘決策樹——我的需求該寫在哪裡？

```
你想監控的是……
│
├─「某資源正被消耗，我要知道多久後耗盡」（磁碟、記憶體、配額、連線數）
│    → capacity_defs/*.yaml        【容量感測 → ETA 引擎】
│      value = 現在用量、ceiling = 上限，引擎預測觸頂時間
│
├─「服務品質承諾，我要知道錯誤預算燒多快」（可用性、延遲）
│    → slo_defs/*.yaml             【SLO 預算感測 → 同一顆 ETA 引擎】
│      sli_query 回傳錯誤率（[0,1]），天花板 = (100−objective)/100
│
└─「這資源閒置很久了，該清理」（零流量 ELB、孤兒 volume）
     → rules.d/*.yaml + labels { sentinel_kind: waste }
       【waste 掃描器 → 候選清單生命週期】
```

三個目錄共同的行為：

- **熱載入**：改檔後下一輪輪詢生效，免重啟。⚠️ macOS Docker Desktop
  （virtiofs）的 fsnotify 事件可能不可達——不可靠時重啟容器
- **best-effort 隔離**：單檔解析失敗只記錯誤日誌並保留舊感測，
  不拖垮其他感測（T028）
- 副檔名僅認 `.yaml` / `.yml`——`.example`、`.bak` 等一律忽略

---

## 2. capacity_defs/*.yaml —— 容量感測

> 欄位定義：`internal/capacity/defs.go` `Def`

### 全欄位表

| 欄位 | 型別 | 必填 | 預設 | 說明 |
|---|---|---|---|---|
| `id` | string | ✅ | — | 感測唯一識別；重複行為未定義，務必唯一 |
| `desc` | string | — | — | 人話描述（顯示用） |
| `metric.value` | PromQL | ✅ | — | 消耗量 m(t)。**必須是 instant vector**——裸 scalar 函式（如 `time()`）要包 `vector()` |
| `metric.ceiling` | PromQL | ✅ | — | 天花板 C(t)，可為動態查詢。同樣需回傳 vector |
| `horizons` | duration 列表 | — | `[1h, 6h, 3d]` | 迴歸視野窗；**激進 ETA 綁定 1h 窗**——自訂 horizons 不含 `1h` 時激進預估恆為空 |
| `thresholds` | 見 §5 | — | 全預設 | 門檻覆寫（四欄皆可選） |
| `service` | string | — | — | 轉交 ai-oncall 時的 `service` label（T020）；gate 四路 collector 以此定位服務 |
| `scope` | string | — | — | 轉交時的 `scope` label（如 cloud / k8s / standalone） |
| `cluster` | string | — | `SENTINEL_CLUSTER_NAME` | 轉交時的 `cluster` label（per-def 覆寫全域環境變數） |

### 範本與實例

- `capacity_defs/node-disk.yaml`——教科書級 used vs size
- `capacity_defs/{memory,cpu,diskio,network,processes}.yaml`——六種基本範本（T033）

### 指標來源對照（key/value 去哪找）

| 想監控 | 指標前綴 | 文件連結 |
|---|---|---|
| 主機 CPU／記憶體／磁碟 I/O／檔案系統／網路 | `node_cpu_*` `node_memory_*` `node_disk_*` `node_filesystem_*` `node_network_*` | [node_exporter README（collectors 一覽＋啟停開關）](https://github.com/prometheus/node_exporter#collectors)、[預設啟用清單](https://github.com/prometheus/node_exporter#enabled-by-default) |
| 抓取目標存活 | `up` | [Prometheus：jobs & instances](https://prometheus.io/docs/concepts/jobs_instances/) |
| K8s 資源物件狀態 | `kube_deployment_*` `kube_node_status_*` `kube_pod_*` | [kube-state-metrics 文件目錄](https://github.com/kubernetes/kube-state-metrics/tree/main/docs) |
| K8s PVC 使用量 | `kubelet_volume_stats_*` | [Kubernetes 測量指標參考（kubelet 區段）](https://kubernetes.io/docs/reference/instrumentation/metrics/) |
| AWS EC2/EBS/ALB/ASG（CloudWatch 轉存） | `aws_ec2_*` `aws_ebs_*` `aws_alb_*` `aws_autoscaling_group_*` | [yace（yet-another-cloudwatch-exporter）](https://github.com/nerdswords/yet-another-cloudwatch-exporter)、[ALB CloudWatch 指標官方清單](https://docs.aws.amazon.com/elasticloadbalancing/latest/application/load-balancer-cloudwatch-metrics.html)、[EBS BurstBalance 語意](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/monitoring-volume-status.html) |
| 阿里雲資源 | 依 CloudMonitor 匯出器而定 | [CloudMonitor 指標文件](https://help.aliyun.com/document_detail/163516.html)、[SLB 監控指標](https://help.aliyun.com/document_detail/35624.html) |

> 不同 exporter 版本的指標名可能略有差異——接線前先用
> `curl -s http://<exporter>/metrics | grep <關鍵字>` 或在 Prometheus
> 的 /targets、/api/v1/label/__name__/values 確認實際序列存在。

---

## 3. slo_defs/*.yaml —— SLO 預算感測

> 欄位定義：`internal/spec/spec.go` `SLO`；格式相容 OpenSLO 子集。

### 全欄位表

| 欄位 | 型別 | 必填 | 預設 | 說明 |
|---|---|---|---|---|
| `id` | string | ✅ | — | 唯一識別 |
| `service` | string | — | — | 服務名；轉交分診時作為 `service` label |
| `description` | string | — | — | 人話描述 |
| `sli_query` | PromQL | ✅ | — | SLI 查詢，**值域契約 [0,1]（1−錯誤率或不良比）**；引擎以 Value/Ceiling 對齊錯誤預算比 |
| `objective` | float | ✅ | — | 目標百分比，開區間 (0, 100)，如 99.9 |
| `window_days` | int | — | `28` | 錯誤預算計算視窗天數；0 或缺省 = 28，負值報錯 |
| `budget_usd` | float | — | `0` | 月度預算天花板（cost 家族選配） |
| `thresholds` | 見 §5 | — | 全預設 | 觸發門檻覆寫（T023，四欄皆可選） |

### 啟動即報錯的非法組合

- 缺 `id` / `sli_query`
- `objective ∉ (0, 100)`
- `window_days < 0`
- thresholds：`soft_ratio ≥ crit_ratio` 或 `warn_eta ≤ crit_eta`

### 參考連結

| 主題 | 連結 |
|---|---|
| OpenSLO 規格（相容子集） | https://github.com/OpenSLO/OpenSLO |
| Google SRE——錯誤預算章節 | https://sre.google/sre-book/service-level-objectives/ |
| Sloth（從 SLO 定義生成 Prometheus rules 的另一條路） | https://github.com/slok/sloth |

---

## 4. rules.d/*.yaml —— 感測目錄（Prometheus rules 格式）

> 格式即標準 Prometheus rules；sentinel 只處理帶分類路由的子集。
> 分類邏輯全解見 [rule-classification.md](rule-classification.md)。

### sentinel 專屬 labels／annotations 全表

| key | 放哪 | 用途 | 家族 | 實作位置 |
|---|---|---|---|---|
| `sentinel_kind` | labels | 明確指定家族：`waste` / `budget` / `capacity`；人工覆寫優先於自動分類 | 全部 | `catalog/classify.go` |
| `sentinel_sensor` | labels 或 annotations | 感測 id（覆寫 alert/record 名） | waste、capacity | `catalog/types.go ID()` |
| `notify_every` | labels | waste 候選重提週期（Prometheus duration，如 `7d`；0 = 只提一次） | waste | `types.go NotifyEvery()` |
| `sentinel_exclude_namespaces` | labels | 排除命名空間（逗號分隔，支援 `openshift-*` 萬用字尾） | waste（K8s） | `types.go ExcludeNamespaces()` |
| `annotations.summary` | annotations | 候選通知的人話理由 | waste | `waste.Scanner.Scan` |
| `sentinel_price_family` | labels | 查價家族：`ec2`/`ebs`/`rds` 或阿里雲 module code（T027） | waste | `pricing.Catalog.Quote` |
| `sentinel_price_attrs` | labels | JSON 屬性 `{region, instance_type, quantity…}`；quantity＝數量倍率 | waste | 同上 |
| labels 前綴 `sloth_` | labels | Sloth 生成慣例 → 自動歸 budget | budget | 自動分類 |

### 自動分類判定線索（無 sentinel_kind 時）

依序比對（命中即停）：annotations 描述含閒置/孤兒語意 → waste；
expr 含天級 `_over_time([30d])` 回看窗 → waste；
expr 涉及 idle/orphan/stale → waste；
`for ≥ 7 天`（400h 以上長窗）→ waste；labels 帶 `sloth_` → budget；
其餘 → capacity 或 none。完整規則見 `internal/catalog/autoclassify.go`
與 [rule-classification.md §2](rule-classification.md)。

### waste 候選的成立條件

`for` 視窗內 expr 序列**全部 ≥ 0.5** 才成立（布林語意的持續成立）。
expr 由 Prometheus 端求值——sentinel 只讀結果序列，不做運算。
規則本身仍可同時掛在真實 Prometheus 觸發 AlertManager 告警，兩者獨立。

### 參考連結

| 主題 | 連結 |
|---|---|
| Prometheus rules 設定格式 | https://prometheus.io/docs/prometheus/latest/configuration/recording_rules/ 、https://prometheus.io/docs/prometheus/latest/configuration/alerting_rules/ |
| `for` 語意 | https://prometheus.io/docs/prometheus/latest/configuration/alerting_rules/#templating |
| Sloth 生成的 rules 範例 | https://github.com/slok/sloth/tree/master/examples |

---

## 5. 門檻與狀態機速查（兩家族通用）

門檻欄位（capacity 的 `thresholds`／SLO 的 `thresholds` 四欄皆可選）：

| 欄位 | 預設 | 說明 |
|---|---|---|
| `warn_eta` | `72h` | warning：ETA 低於此值 |
| `crit_eta` | `6h` | critical：激進 ETA 低於此值 |
| `soft_ratio` | `0.80` | 提前警告水位（使用率） |
| `crit_ratio` | `0.95` | critical 水位 |

驗證規則（違反即拒絕）：`soft_ratio < crit_ratio` 且 `warn_eta > crit_eta`。

狀態判定（任一命中即成立，詳見 engine-guide §4）：
warning = 激進 ETA < warn_eta 且 U ≥ soft_ratio，或穩健 ETA < warn_eta；
critical = 激進 ETA < crit_eta，或 U ≥ crit_ratio；
healthy 需**連續 2 輪**低於門檻才降級（解除遲滯，防抖動）。

熱載入副作用：重建感測器會重置解除遲滯計數與前次天花板快取。

---

## 6. 轉交 ai-oncall 的 label 契約（T020 映射表）

容量 warning/critical 轉交時的 alert payload（AM webhook 格式，
Bearer token 即 gate 的 SHARED_SECRET）：

| sentinel label | 來源 | ai-oncall 用途 |
|---|---|---|
| `alertname=CapacityEtaWarning` | 固定值 | gate 透傳；分診報告識別 |
| `sensor_id` | def `id` | gate 透傳（人讀） |
| `severity` | 引擎 state：warning/critical；resolved 時 info | gate 映射 CRITICAL/WARNING/INFO |
| `service` | def `service`（SLO 家族則為 spec service） | **gate 四路 collector 定位服務的唯一鍵**；缺省降級全域查詢 |
| `cluster` | def `cluster` 或 `SENTINEL_CLUSTER_NAME` | 多叢集分流：gate 依此選擇 Prometheus 端點（ai-oncall T022）；未知 cluster 退回預設端點 |
| `eta_aggressive` / `eta_conservative` | 引擎輸出秒數 | gate 透傳，供報告引用 |

啟用方式：設定 `ONCALL_GATE_URL`（+`ONCALL_GATE_TOKEN`）。
轉交成功 → 本地精簡卡；失敗 → 回退完整本地卡（critical 不丟失優先）。

---

## 7. 常見錯誤對照表（實戰教訓萃取）

| 症狀 | 原因 | 解法 |
|---|---|---|
| `cannot unmarshal number into .data.result` | expr 回傳 scalar（如裸 `time()`），parser 只吃 vector | 包一層：`vector(time())` |
| 激進預估永遠「無成長跡象」 | 自訂 `horizons` 不含 `1h`——激進綁定最短窗的實作是硬編碼 `w == time.Hour` | horizons 加回 `1h`，或等待引擎改為「取最短窗」 |
| UI 部分欄位空白 | API JSON 鍵名 vs UI 期待不一致（歷史列無值） | 已修（`6a5d925`）；歷史列不追溯填補 |
| 目錄版本大多為空 | 該列寫入時 `CatalogVersion` 尚未實作（`6a5d925` 之前） | 同上，新列起才有值 |
| 改了 def 但沒生效（macOS） | virtiofs 的 fsnotify 事件不可靠 | 重啟容器；Linux 正常 |
| thresholds 非法組合啟動失敗 | `soft ≥ crit` 或 `warn_eta ≤ crit_eta` | 修正組合；錯誤訊息會指名欄位 |
| node_exporter 相關感測輪詢報錯 | 該 Prometheus 沒有 node_exporter 資料 | best-effort 設計：不影響其他感測；不需要就移除該檔 |
| waste 金額欄全為「—」 | 未設定 `sentinel_price_family` 或 pricing 查價失敗 | 加 price labels；離線時金額留空是設計行為（不虛構） |

---

## 8. 欄位 ↔ 範本／測試索引

| 欄位/功能 | 範本檔 | 測試 |
|---|---|---|
| metric.value/ceiling、horizons | `capacity_defs/node-disk.yaml` | `cmd/sentinel/e2e_test.go` |
| 六種基本範本 | `capacity_defs/{memory,cpu,diskio,network,processes}.yaml` | `TestBasicSensorTemplatesParse` |
| thresholds 覆寫 | `docs/engine-budget-capacity.md` §6 | `config`/`spec` 測試 |
| slo thresholds 非法組合 | — | `TestLoadInvalidThresholdCombinations` |
| waste price labels | `internal/waste/pricing_test.go` fixture | `TestScanAttachesMonthlySaving` 等 |
| .example 不被載入 | `slo_defs/TEMPLATE.*.yaml.example` | `TestLoadIgnoresExampleTemplates`、`TestTemplateFilesAreValidYAML` |
| 分诊 label 契約 | `cmd/sentinel/triage_test.go` | `TestTriagePublishesAMFormatAlert` 等 |
| 熱載入 | — | `internal/watch/watch_test.go`、`TestSetupSensorsKeepsOldOnBadDefs` |

---

## 9. 附贈：讓 AI 幫你寫定義檔的 Prompt

> 把下面兩段存成常用片段。**效果最好的用法**：把本手冊全文一起貼給 AI 當
> context，再貼生成 prompt——AI 有完整欄位契約就能一次寫對。

### 9.1 生成用 Prompt（複製後填入【】處）

```text
你熟悉 Prometheus 與 slo-sentinel（一個宣告式 SLO/容量感測守護程序）。
請依下列契約，幫我產生【capacity_defs / slo_defs / rules.d】的設定檔。

【我的需求】
- 監控對象：【例：prod 環境的 PostgreSQL 主機群】
- 我在意的資源/品質：【例：磁碟用量、連線數接近上限、5xx 比例】
- 指標來源：【例：node_exporter＋postgres_exporter，已接進 Prometheus；
  指標前綴 pg_】
- 服務名／標籤慣例：【例：service=pg-main，cluster=aws-prod】
- 告警個性：【例：早點吵；或：穩健優先少誤報】

【schema 契約（必須嚴格遵守）】
1. capacity 家族（capacity_defs/*.yaml）：sensors 列表，欄位
   id/desc/metric.value/metric.ceiling/horizons/thresholds/service/scope/cluster。
   value 與 ceiling 都必須是「回傳 instant vector 的 PromQL」——
   裸 scalar 函式要包 vector()。value 是消耗量、ceiling 是上限，
   引擎預測 value 何時觸及 ceiling。
2. slo 家族（slo_defs/*.yaml）：slos 列表，欄位
   id/service/sli_query/objective/window_days/thresholds。
   sli_query 值域契約 [0,1]（錯誤率或不良比）；objective 是百分比如 99.9；
   window_days 預設 28。
3. waste 家族（rules.d/*.yaml，Prometheus rules 格式）：alert rule ＋
   labels { sentinel_kind: waste, sentinel_sensor: <id>, notify_every: <7d> }，
   expr 必須是「布林持續成立」型（如 max_over_time(x[14d]) <= 10），
   for ≥ 14d，annotations.summary 寫人話理由。
4. thresholds 四欄（可選）：warn_eta/crit_eta/soft_ratio/crit_ratio，
   預設 72h/6h/0.80/0.95；驗證規則 soft < crit 且 warn_eta > crit_eta，
   違反會被拒絕載入。
5. 全部註解用繁體中文，說明每條 expr 的業務語意與調整點。
6. 不確定指標是否存在時，明確告訴我「先驗證這條查詢」而不是假裝能動。

【輸出要求】
- 只輸出 YAML（可含中文註解），每檔一個程式碼區塊，檔名建議附上
- 輸出後附自我檢查清單：逐項確認上述契約第 1–6 點
```

### 9.2 審查用 Prompt（讓 AI 當 reviewer 再驗一遍）

```text
以下是我（或另一個 AI）寫的 sentinel 定義檔。請當審查者逐項檢查：

1. 欄位拼字與層級符合 schema（capacity: sensors/metric.value/metric.ceiling；
   slo: slos/sli_query/objective）
2. 所有 PromQL 是否為合法且回傳 instant vector？有沒有裸 scalar？
3. slo 的 sli_query 值域是否真的落在 [0,1]？（除法分母為 0 時會怎樣？）
4. thresholds 組合是否合法（soft < crit、warn_eta > crit_eta）？
5. capacity 的 ceiling 是否可能小於 value（開機即 critical）？
6. 波動型資源（記憶體/CPU）是否誤用了確定成長的語意？
7. 分診相關欄位（service/scope/cluster）是否按需攜帶？

輸出格式：問題列表（檔案/行/原因/建議修法），最後給 PASS 或 NEEDS FIX。
【貼上設定檔】
```

### 9.3 使用心法

1. **先生成 → 再審查 → 最後實測**三段式；審查那段抓到的問題通常比生成多
2. 實測的最快路徑：放進 `capacity_defs/` 後看
   `docker logs slo-sentinel | grep sensor_poll_failed`——
   查詢失敗會在下一輪就現形
3. AI 不知道你的 Prometheus 裡有哪些序列。把
   `/api/v1/label/__name__/values` 的輸出（或 grep 過的子集）貼給它，
   正確率會大幅提升
