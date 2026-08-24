# E2E 分診演練報告——容量預警接 ai-oncall 閉環（T020 驗收 3）

- **日期**：2026-08-26
- **執行方式**：本機 Docker Desktop，`scripts/e2e-triage-drill.sh` 一鍵全鏈路
- **結論**：**演練成功**。容量 critical 事件從 sentinel 感測、AM webhook 轉交、
  gate 正規化建檔、core 分診管線到報告產出全程打通；本地精簡卡與冪等去重
  同步驗證。T020 四項驗收全數滿足。

---

## 1. 演練環境

| 元件 | 版本/來源 | 執行位置 |
|---|---|---|
| slo-prometheus | `prom/prometheus:latest`（dev profile） | slo-sentinel compose |
| slo-node-exporter | `prom/node-exporter:latest`（dev profile） | slo-sentinel compose |
| slo-sentinel | 本 repo commit `852a588` 建置（含 T020/T024–T029） | slo-sentinel compose |
| oncall-gate / oncall-core | ai-oncall commit `bd15991` 建置 | ai-oncall deploy compose |
| mock LLM | `scripts/mock_llm.py`（OpenAI 相容假端點，回傳 schema 合法報告） | host `127.0.0.1:18000` |

跨 stack 網路：容器一律經 `host.docker.internal` 存取對方發布到本機的埠
（gate :8080、prometheus :9090、mock LLM :18000），兩個 compose stack 零改造。

## 2. 觸發設計：確定論的 critical

真實磁碟在演練時間尺度內沒有成長量，ETA 引擎會判定斜率≈0（healthy）。
因此腳本將 `capacity_defs/node-disk.yaml` 暫時替換為合成感測：

```yaml
sensors:
  - id: dev-root-disk
    service: storage-api        # T020：ai-oncall 定位服務的唯一鍵
    scope: k8s
    metric:
      value:   'vector(time())'   # 每秒 +1 的確定成長序列
      ceiling: 'vector(<now+240>)'# 固定天花板：4 分鐘後觸頂
    horizons: [6h]
    thresholds:
      warn_eta: 90s
      crit_eta: 45s
```

- `vector()` 包裝是必要的：裸純量回應會讓 query parser 反序列化失敗
  （第一次演練的教訓，見 §5）
- 結束時腳本自動還原原檔

## 3. 全鏈路與實證

```
node-exporter → prometheus → sentinel（ETA 引擎判定 critical）
    │ POST http://gate:8080/alerts（Bearer ONCALL_GATE_TOKEN）
    ▼
oncall-gate：認證 → 冪等檢查 → Normalize → AlertEvent
    │ gRPC ReportIncident
    ▼
oncall-core：incident 建檔 → CollectContext fan-out → LLM 分診（mock）
    → schema 驗證 → triage_completed → readapi /api/incidents
```

### 3.1 sentinel 判定與轉交

```json
{"states":{"dev-root-disk":{"sensor_id":"dev-root-disk",
 "state":"critical","last_value":1787589716.402,...}}}
```

轉交後無任何 `triage_publish_failed` 錯誤日誌。

### 3.2 gate 收到並正規化（incident 建立）

```json
{"id": "inc-c35c49c0e836", "fingerprint": "derived-f15c714d334e5736",
 "status": "open", "severity": 3,
 "labels": {"alertname": "CapacityEtaWarning",
            "scope": "k8s", "sensor_id": "dev-root-disk",
            "service": "storage-api", "severity": "critical",
            "eta_conservative": "238"}}
```

- `severity=critical` 正確映射為 CRITICAL（enum 3）
- labels 全數透傳保留；fingerprint 由 labels SHA-256 自動導出

### 3.3 core 分診報告（triage_completed）

timeline 出現 `incident_created` → `triage_completed` 兩個事件，
prediction（通過 schema 驗證的 TriageReport）：

```json
[{"cause": "容量成長速率超過現有規劃（sentinel ETA 觸頂預警，E2E 演練）",
  "confidence": 0.85,
  "evidence": ["labels: alertname=CapacityEtaWarning severity=critical",
                "labels: service 未攜帶",
                "context: Prometheus availability/request_rate 序列（見 context bundle）"]},
 {"cause": "HPA 擴容上限或 quota 不足導致無法消化流量",
  "confidence": 0.4,
  "evidence": ["kube_deployment_status_replicas 若缺漏會列於 degraded_sources"]}]
```

> 附註：evidence 中「service 未攜帶」是 mock LLM 從 prompt 抽取文字的
> 小瑕疵——incident labels 實際帶有 `service=storage-api`（見 3.2），
> 展示層資料正確。

### 3.4 本地精簡卡（去重協調，功能設計 3）

```
[notify] 📨 dev-root-disk critical — 已轉交 ai-oncall 分診
```

完整卡（雙視野 ETA 文字）未在本地重複推播——同一事件只有分診管線裡那一份長文。

### 3.5 附帶驗證到的行為

| 行為 | 實證 |
|---|---|
| resolved 轉交關閉 incident | 前輪 critical 恢復後，gate 收到 `status=resolved`、severity=info 的第二筆事件並建檔 |
| gate 冪等去重（E.2） | 容器重建後同 labels 重複轉交 → 同 fingerprint → 不重複建 incident |
| T028 熱載入 | 日誌出現 `defs_hot_reloading`／`sensors_configured`；但 macOS virtiofs 事件不可靠，腳本改以 `--force-recreate` 保證載入（見 §5） |
| T026 失敗回退 | gate 404 故意情境中，本地照發完整卡（critical 不丟失優先） |

## 4. 驗收標準對照（T020）

| # | 驗收標準 | 結果 |
|---|---|---|
| 1 | 前置條件逐一驗證並記錄 | ✅ 任務書「前置條件驗證紀錄」（ai-oncall T001–T021 done；標籤契約由程式碼凍定＋T022 cluster 分流） |
| 2 | alert payload 通過 AM 相容性測試 | ✅ 離線以 schema 鏡像斷言＋本次實際送入 gate 成功建檔（amtool 未安裝，已如實標注） |
| 3 | 端到端演練：critical → 收到 → 分診報告 | ✅ 本報告 §3 |
| 4 | 去重協調：進入分診者本地不重複長文 | ✅ §3.4 |

## 5. 演練過程發現並修復的問題

| # | 問題 | 修復 | commit |
|---|---|---|---|
| 1 | ai-oncall compose 未傳 `LLM_PROVIDERS`——core 永遠離線模式只建檔不分診 | compose core 服務補環境變數傳遞 | ai-oncall `bd15991` |
| 2 | executor `AUDIT_DIR` 相對路徑在唯讀工作目錄炸 PermissionError（core 崩潰循環） | compose 設 `AUDIT_DIR=/data/audit`（可寫資料卷） | ai-oncall `bd15991` |
| 3 | macOS virtiofs fsnotify 事件偶發不可達，熱載入不保證觸發 | 演練腳本改用 `--force-recreate`；正式文件已標注此限制 | slo-sentinel `302bf8e` |
| 4 | 裸 scalar PromQL（`time()`）回應格式讓 query parser 反序列化失敗 | 合成感測改寫 `vector(time())`；引擎本身無需修改 | slo-sentinel `302bf8e` |
| 5 | `docker compose down -v` 因 `${SHARED_SECRET:?}` 插值失敗被静默吞掉，舊狀態殘留 | 腳本 export 變數後再 down | slo-sentinel `302bf8e` |

## 6. 重現方式

```bash
# 前置：Docker Desktop 已啟動；ai-oncall 在 ~/Projects/ai-oncall
cd ~/Projects/slo-sentinel
scripts/e2e-triage-drill.sh          # FORCE_CRITICAL=1 預設開啟

# 查驗點
curl -s http://127.0.0.1:9099/api/status.json          # dev-root-disk = critical
docker logs slo-sentinel 2>&1 | grep '\[notify\]'      # 📨 已轉交精簡卡
curl -s http://127.0.0.1:8090/api/incidents            # incident + triage_completed
```

收尾：

```bash
docker compose --profile dev down          # slo-sentinel（資料卷保留）
cd ~/Projects/ai-oncall/deploy && docker compose down -v
```
