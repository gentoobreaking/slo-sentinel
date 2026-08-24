# sentinel-gen 使用手冊——AI 協作產生／審查／驗證定義檔

> 定義檔欄位的權威參考見 [definitions-guide.md](definitions-guide.md)；
> 本手冊只講工具本身怎麼用。

`sentinel-gen` 是一個獨立 CLI（與 daemon 同倉同語言），把「AI 協作寫設定檔」
固化為四個子命令。核心設計：**審查層直接重用 daemon 的生產解析器**
（`capacity.LoadDefs` / `spec.Load` / `catalog.Loader`）——能通過 review 的檔案，
daemon 就一定載得動。

```
generate ──→ 候選 YAML ──→ review ──┬─ 靜態 schema（生產解析器）
                                    ├─ live expr 檢查（真實 Prometheus）
                                    └─ LLM 第二意見（可選）
         有問題時 ←─ fix（問題回餵 LLM，≤3 輪，自動 .bak）
         
verify = 套用前最終關卡：靜態必過 + 每條 expr 打真實 Prometheus → READY TO APPLY
```

---

## 1. 安裝

```bash
make build        # 產出 bin/sentinel-gen
# 或單獨建：
go build -o bin/sentinel-gen ./cmd/sentinel-gen
```

## 2. 環境變數（LLM 端點）

| 變數 | 說明 |
|---|---|
| `GEN_LLM_URL` | OpenAI 相容端點 base_url。**任何**支援 `/chat/completions` 的服務：OpenAI、DeepSeek、Groq、vLLM、Ollama（`http://127.0.0.1:11434/v1`）、LM Studio… |
| `GEN_LLM_KEY` | API key；本地端點填任意值即可 |
| `GEN_LLM_MODEL` | model 名 |

```bash
# 雲端範例（DeepSeek）
export GEN_LLM_URL=https://api.deepseek.com/v1
export GEN_LLM_KEY=sk-...
export GEN_LLM_MODEL=deepseek-chat

# 完全離線範例（本機 Ollama）
export GEN_LLM_URL=http://127.0.0.1:11434/v1
export GEN_LLM_MODEL=llama3
export GEN_LLM_KEY=ollama
```

> 未設定環境變數時：`generate` / `fix` 明確報錯；
> `review` / `verify` 的靜態與 live 層**照常運作**（LLM 層只是第二意見）。

## 3. 子命令

### 3.1 generate —— 產生候選定義檔

```bash
sentinel-gen generate \
  -kind capacity \
  -desc "監控 prod 環境 PostgreSQL 主機群的磁碟用量與連線數" \
  -extra "指標前綴 pg_；已確認存在 pg_stat_database_numbackends" \
  -out pg-capacity.yaml
```

| 旗標 | 必填 | 說明 |
|---|---|---|
| `-kind` | ✅ | `capacity` ／ `slo` ／ `waste` |
| `-desc` | ✅ | 自然語言需求描述——越具體越好（資源、環境、告警個性） |
| `-out` | — | 輸出檔案路徑；省略則印到 stdout |
| `-extra` | — | 補充線索：指標前綴、已知序列名、label 慣例等 |

產出的 YAML 只是**候選**——必須再走 review / verify。

### 3.2 review —— 三層品質審查

```bash
sentinel-gen review -file pg-capacity.yaml
sentinel-gen review -file pg-capacity.yaml -llm=false   # 只跑靜態層
sentinel-gen review -file pg-capacity.yaml -kind slo    # 手動指定家族
```

三層依序執行：

1. **靜態 schema**：用 daemon 同款解析器載入——缺 id、objective 超界、
   thresholds 非法組合（soft ≥ crit、warn_eta ≤ crit_eta）等都會被攔截，
   問題訊息指名欄位
2. **live expr 檢查**（需 `-prom`，見 verify）：可選
3. **LLM 第二意見**：契約點檢（instant vector 陷阱、值域 [0,1]、除零防護…），
   回覆含 `NEEDS FIX` 即計入問題

### 3.3 verify —— 套用前最終關卡

```bash
sentinel-gen verify -file pg-capacity.yaml -prom http://prometheus:9090
```

| 旗標 | 說明 |
|---|---|
| `-file` | ✅ 定義檔 |
| `-prom` | ✅ 套用目標的 Prometheus 端點 |
| `-kind` | 可選，自動嗅探 |

流程：靜態層必須先全過 → 收集檔內每條 expr（capacity 的 value+ceiling、
slo 的 sli_query、waste 的各條 alert expr）逐一打真實 Prometheus：

| 結果 | 判定 |
|---|---|
| `resultType=vector` 且有序列 | ✅ 通過 |
| `resultType=scalar` | ❌ 裸 scalar 函式陷阱（如忘記包 `vector(time())`） |
| vector 但序列為空 | ⚠️ 該 Prometheus 沒有此資料源——套用後會輪詢失敗 |

全部通過輸出 **READY TO APPLY**（exit 0）。這是「產生的 rules 可以直接套用」
的可機器判定結論。

### 3.4 fix —— 自動修復迴圈

```bash
sentinel-gen fix -file pg-capacity.yaml            # 預設最多 3 輪
sentinel-gen fix -file pg-capacity.yaml -rounds 5
```

流程：靜態驗證 → 有問題就連同問題列表回餵 LLM 重寫 → 再驗 → 循環。
原檔每次覆寫前自動備份為 `<file>.bak`。超過輪數仍有問題 → 如實報錯，
請人工處理。

## 4. 推薦工作流

```bash
# 全自動
sentinel-gen generate -kind slo -desc "…" -out s.yaml \
&& sentinel-gen review -file s.yaml && sentinel-gen verify -file s.yaml -prom http://…

# 半自動（AI 生成 + 人類把關）
sentinel-gen generate … -out draft.yaml
$EDITOR draft.yaml                     # 人先看過改過
sentinel-gen review -file draft.yaml -llm=false
sentinel-gen verify   -file draft.yaml -prom http://…
```

通過 verify 後放進正式目錄（`capacity_defs/` 等），熱載入即生效。

## 5. 接進 CI（選配）

exit code 已語意化（0 通過／1 問題／2 用法錯誤），可直接當 gate：

```yaml
- name: validate sensor definitions
  run: ./bin/sentinel-gen verify -file ${{ matrix.def }} -prom http://prometheus:9090
```

## 6. 常見問題

| 症狀 | 原因/處理 |
|---|---|
| `需要設定 GEN_LLM_URL 與 GEN_LLM_MODEL` | generate/fix 需要 LLM；只想驗證就用 review/verify |
| review 報「沒有規則被分類為 waste」 | rules.d 格式忘了 `labels: { sentinel_kind: waste }` |
| verify 報 scalar | expr 用了裸 scalar 函式（`time()` 等）——包 `vector()` |
| verify 序列為空 | 目標 Prometheus 沒有該指標——確認 exporter 有掛、job 名正確 |
| LLM 審查一直 NEEDS FIX | LLM 審查是建議性質；以靜態+live 層為準，人工判讀其餘建議 |
