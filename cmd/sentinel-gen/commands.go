package main

// commands.go：四個子命令的實作。

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
)

// ---- generate -----------------------------------------------------------

func cmdGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	kind := fs.String("kind", "capacity", "家族：capacity | slo | waste")
	desc := fs.String("desc", "", "需求描述（自然語言）")
	out := fs.String("out", "", "輸出檔案；空 = 印到 stdout")
	extra := fs.String("extra", "", "補充線索：指標前綴、已知序列等")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg := loadEnv()
	if err := cfg.requireLLM(); err != nil {
		return err
	}
	if *desc == "" {
		return fmt.Errorf("-desc 必填：用自然語言描述你要監控什麼")
	}

	ctx := context.Background()
	text, err := newLLM(cfg).complete(ctx, generateSystem(), generateUser(*kind, *desc, *extra))
	if err != nil {
		return err
	}
	yamlOut := extractYAML(text)
	if strings.TrimSpace(yamlOut) == "" {
		return fmt.Errorf("LLM 輸出未包含 YAML 內容")
	}
	target := *out
	if target == "" {
		fmt.Println(yamlOut)
		return nil
	}
	if err := os.WriteFile(target, []byte(yamlOut), 0o644); err != nil {
		return err
	}
	fmt.Printf("✅ 已寫入 %s\n下一步建議：sentinel-gen review -file %s && sentinel-gen verify -file %s -prom <URL>\n",
		target, target, target)
	return nil
}

// ---- review -------------------------------------------------------------

func cmdReview(args []string) error {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	file := fs.String("file", "", "定義檔路徑")
	kind := fs.String("kind", "", "capacity | slo | waste（未指定自動嗅探）")
	useLLM := fs.Bool("llm", true, "LLM 端點已設定時進行第二意見審查")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("-file 必填")
	}
	content, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	k := *kind
	if k == "" {
		k = detectKind(string(content))
	}
	fmt.Printf("家族判定：%s\n--- 靜態 schema 驗證 ---\n", k)

	iss, err := staticValidate(*file, k)
	if err != nil {
		return err
	}
	for _, i := range iss {
		fmt.Println(i.String())
	}
	errs := countErrors(iss)
	fmt.Printf("靜態層：%d 個問題\n", errs)

	// LLM 第二意見（可選）
	if *useLLM {
		if cfg := loadEnv(); cfg.URL != "" {
			fmt.Println("--- LLM 第二意見 ---")
			ctx := context.Background()
			text, lerr := newLLM(cfg).complete(ctx, reviewSystem(), reviewUser(k, string(content)))
			if lerr != nil {
				fmt.Printf("[WARN] LLM 審查失敗（不影響靜態結論）：%v\n", lerr)
			} else {
				fmt.Println(strings.TrimSpace(text))
				if strings.Contains(text, "NEEDS FIX") {
					errs++
				}
			}
		}
	}

	if errs > 0 {
		return fmt.Errorf("review 發現 %d 個問題", errs)
	}
	fmt.Println("✅ review PASS")
	return nil
}

// ---- verify -------------------------------------------------------------

// 驗收：scalar／空 vector／正常 vector 三種形狀——僅正常 vector 通過。
func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	file := fs.String("file", "", "定義檔路徑")
	prom := fs.String("prom", "", "Prometheus 端點（套用目標，必填）")
	kind := fs.String("kind", "", "家族（可選，自動嗅探）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" || *prom == "" {
		return fmt.Errorf("-file 與 -prom 必填")
	}
	content, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	k := *kind
	if k == "" {
		k = detectKind(string(content))
	}

	// 第一關：靜態必須先過（能被 daemon 同款解析器載入）
	iss, err := staticValidate(*file, k)
	if err != nil {
		return err
	}
	errs := countErrors(iss)
	for _, i := range iss {
		fmt.Println(i.String())
	}
	if errs > 0 {
		return fmt.Errorf("verify：靜態層 %d 個問題，先修再驗", errs)
	}

	// 第二關：每條 expr 打真實 Prometheus
	exprs, _, err := collectExprs(k, *file)
	if err != nil {
		return err
	}
	if len(exprs) == 0 {
		return fmt.Errorf("verify：沒有可驗證的 expr")
	}
	ctx := context.Background()
	failed := 0
	fmt.Println("--- live 驗證（真實 Prometheus instant query）---")
	for _, e := range exprs {
		res, lerr := liveCheckExpr(ctx, *prom, e.Expr)
		switch {
		case lerr != nil:
			failed++
			fmt.Printf("  ❌ %s：%v\n", e.Desc, lerr)
		case res.ResultType != "vector":
			failed++
			fmt.Printf("  ❌ %s：回傳 resultType=%q（需要 vector——裸 scalar 函式要包 vector()）\n",
				e.Desc, res.ResultType)
		case res.Series == 0:
			failed++
			fmt.Printf("  ⚠️ %s：vector 但序列為空（該 Prometheus 沒有此資料源？）\n", e.Desc)
		default:
			fmt.Printf("  ✅ %s：vector，%d 序列\n", e.Desc, res.Series)
		}
	}
	if failed > 0 {
		return fmt.Errorf("verify 失敗：%d 條 expr 未通過——尚不可套用", failed)
	}
	fmt.Printf("✅ READY TO APPLY：%d 條 expr 全部通過（家族 %s）\n", len(exprs), k)
	return nil
}

// ---- fix ----------------------------------------------------------------

const fixMaxRounds = 3

func cmdFix(args []string) error {
	fs := flag.NewFlagSet("fix", flag.ExitOnError)
	file := fs.String("file", "", "定義檔路徑（就地覆寫前會先備份為 .bak）")
	kind := fs.String("kind", "", "家族（可選）")
	maxRounds := fs.Int("rounds", fixMaxRounds, "最大修復輪數")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("-file 必填")
	}
	cfg := loadEnv()
	if err := cfg.requireLLM(); err != nil {
		return err
	}
	client := newLLM(cfg)
	ctx := context.Background()

	var lastIssues []issue
	for round := 1; round <= *maxRounds; round++ {
		content, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		k := *kind
		if k == "" {
			k = detectKind(string(content))
		}
		lastIssues, err = staticValidate(*file, k)
		if err != nil {
			lastIssues = append(lastIssues, issue{"ERROR", *file, err.Error()})
		}
		errs := countErrors(lastIssues)
		fmt.Printf("--- fix 第 %d 輪：%d 個問題 ---\n", round-1, errs)
		if errs == 0 {
			fmt.Printf("✅ %s 通過靜態驗證\n", *file)
			return nil
		}
		var msgs []string
		for _, i := range lastIssues {
			msgs = append(msgs, i.String())
		}
		text, ferr := client.complete(ctx, fixSystem(), fixUser(k, string(content), msgs))
		if ferr != nil {
			return fmt.Errorf("第 %d 輪修復失敗：%w", round, ferr)
		}
		fixed := extractYAML(text)
		if strings.TrimSpace(fixed) == "" {
			return fmt.Errorf("第 %d 輪 LLM 未輸出 YAML", round)
		}
		bak := *file + ".bak"
		if err := os.WriteFile(bak, content, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(*file, []byte(fixed), 0o644); err != nil {
			return err
		}
		fmt.Printf("  已套用修正版（原檔備份於 %s）\n", bak)
	}

	// 最終複驗
	finalIssues, err := staticValidate(*file, *kind)
	if err != nil {
		return err
	}
	if n := countErrors(finalIssues); n > 0 {
		for _, i := range finalIssues {
			fmt.Println(i.String())
		}
		return fmt.Errorf("%d 輪修復後仍有 %d 個問題——請人工處理或重跑", *maxRounds, n)
	}
	fmt.Printf("✅ %d 輪修復後通過驗證\n", *maxRounds)
	return nil
}

// ---- 共用 ----------------------------------------------------------------

func countErrors(iss []issue) int {
	n := 0
	for _, i := range iss {
		if i.Level == "ERROR" {
			n++
		}
	}
	return n
}
