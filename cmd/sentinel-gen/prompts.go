package main

// prompts.go：內嵌的 schema 契約與生成/審查 prompt 建構。
// 濃縮自 docs/definitions-guide.md——手冊是權威，此處是給 LLM 的最小充分集。

import (
	"fmt"
	"strings"
)

const contract = `【sentinel 定義檔 schema 契約】
1. capacity 家族（capacity_defs/*.yaml）：
   sensors:
     - id: <唯一識別，必填>
       desc: <人話描述>
       service: <分診用服務名，可選>      # scope/cluster 亦為選配 label
       metric:
         value:   <PromQL：現在消耗量>    # 必填
         ceiling: <PromQL：上限>          # 必填
       horizons: [1h, 6h, 3d]            # 可選；激進 ETA 綁定 1h 窗
   value/ceiling 都必須回傳 instant vector——裸 scalar 函式（如 time()）
   要包 vector()。引擎預測 value 何時觸及 ceiling。
2. slo 家族（slo_defs/*.yaml）：
   slos:
     - id: <必填>
       service: <可選>
       sli_query: <PromQL 錯誤率，值域 [0,1]，必填>
       objective: <百分比 (0,100)，如 99.9，必填>
       window_days: 28                   # 可選
3. waste 家族（rules.d/*.yaml，Prometheus alert rule 格式）：
   - alert: <名稱>
     expr: <布林持續成立型，如 max_over_time(x[14d]) <= 10>
     for: 14d
     labels: { sentinel_kind: waste, sentinel_sensor: <id>, notify_every: 7d }
     annotations: { summary: "<人話理由>" }
4. thresholds 四欄皆可選（capacity 與 slo 皆有）：warn_eta/crit_eta/
   soft_ratio/crit_ratio，預設 72h/6h/0.80/0.95；
   驗證規則 soft_ratio < crit_ratio 且 warn_eta > crit_eta，違反會被拒絕。
5. 全部註解用繁體中文，說明每條 expr 的業務語意與調整點。
6. 不確定指標是否存在時，明確標注「先驗證這條查詢」，不得假裝能動。`

func generateSystem() string {
	return "你是 Prometheus 監控設定專家，熟悉 slo-sentinel 的宣告式定義檔。" +
		"只輸出 YAML（```yaml 圍欄），並在最後附自我檢查清單逐項確認契約。"
}

func generateUser(kind, desc, extra string) string {
	var family string
	switch kind {
	case "capacity":
		family = "產出 capacity 家族設定（sensors 列表）"
	case "slo":
		family = "產出 slo 家族設定（slos 列表）"
	default:
		family = "產出 waste 家族設定（Prometheus alert rules，帶 sentinel_kind: waste labels）"
	}
	p := fmt.Sprintf("%s\n\n【任務】%s\n【需求描述】\n%s\n", contract, family, desc)
	if extra != "" {
		p += "\n【補充線索（指標前綴、已知序列等）】\n" + extra + "\n"
	}
	return p
}

func reviewSystem() string {
	return "你是 sentinel 定義檔的嚴格審查者。只依下列契約檢查，不猜測未提供的環境資訊。" +
		"輸出格式：問題列表（每行「- [位置] 問題：建議」），最後一行單獨輸出 PASS 或 NEEDS FIX。"
}

func reviewUser(kind, content string) string {
	return fmt.Sprintf("%s\n\n【待審查的 %s 家族定義檔】\n%s\n"+
		"\n請重點點檢查：欄位拼字層級、PromQL 是否 instant vector（裸 scalar 陷阱）、"+
		"slo 的 sli_query 值域 [0,1] 與除零防護、thresholds 組合合法性、"+
		"ceiling 是否可能小於 value、波動型資源是否誤用成長語意。",
		contract, kind, content)
}

func fixSystem() string {
	return "你是 sentinel 定義檔修復者。根據審查問題列表修正 YAML，" +
		"只輸出修正後完整 YAML（```yaml 圍欄），不要解釋。"
}

func fixUser(kind, content string, issues []string) string {
	return fmt.Sprintf("%s\n\n【%s 家族定義檔（有問題）】\n%s\n\n【必須修復的問題】\n%s\n",
		contract, kind, content, strings.Join(issues, "\n"))
}
