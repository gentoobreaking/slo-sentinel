// sentinel-gen（T036）：AI 協作產生／審查／驗證 sentinel 定義檔的 CLI。
//
// 用法：
//
//	sentinel-gen generate -kind capacity -desc "監控 prod PG 磁碟用量" [-out f.yaml]
//	sentinel-gen review   -file f.yaml            # 靜態 schema ＋ LLM 第二意見（可選）
//	sentinel-gen verify   -file f.yaml -prom URL  # 套用前最終關卡（真實 Prometheus）
//	sentinel-gen fix      -file f.yaml            # review 問題回餵 LLM 修一版
//
// 環境變數：GEN_LLM_URL / GEN_LLM_KEY / GEN_LLM_MODEL（OpenAI 相容端點）。
// 未設定 LLM 時 generate/fix 報錯，review/verify 的靜態與 live 層照常運作。
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = cmdGenerate(os.Args[2:])
	case "review":
		err = cmdReview(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "fix":
		err = cmdFix(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `sentinel-gen — AI 協作產生/審查/驗證 sentinel 定義檔

用法：
  sentinel-gen generate -kind capacity|slo|waste -desc "…" [-out file.yaml]
  sentinel-gen review   -file file.yaml [-llm]
  sentinel-gen verify   -file file.yaml -prom http://prometheus:9090
  sentinel-gen fix      -file file.yaml

環境變數：
  GEN_LLM_URL    OpenAI 相容端點 base_url（如 http://127.0.0.1:11434/v1）
  GEN_LLM_KEY    API key（本地端點可任意值）
  GEN_LLM_MODEL  model 名稱

exit code：0 全部通過；1 有問題；2 用法錯誤。
`)
}
