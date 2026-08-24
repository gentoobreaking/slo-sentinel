// ruleclassify——對 rules 檔（目錄）執行社群規則自動分類的小工具。
//
// 由 scripts/sync-community.sh 在複製完挑選的服務規則後呼叫；
// 亦可獨立使用：go run ./cmd/ruleclassify rules.d/community
package main

import (
	"flag"
	"fmt"
	"log"

	"slo-sentinel/internal/catalog"
)

func main() {
	dir := flag.String("dir", "", "要自動分類的 rules 目錄（遞迴）")
	flag.Parse()
	if *dir == "" {
		log.Fatal("用法：ruleclassify -dir <rules 目錄>")
	}
	files, rules, err := catalog.AutoClassifyDir(*dir)
	if err != nil {
		log.Fatalf("自動分類失敗: %v", err)
	}
	fmt.Printf("✅ 自動分類完成：掃描 %d 個檔案，補上 %d 條 sentinel_kind 標籤\n", files, rules)
}
