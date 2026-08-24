package main

// status.go（T009）：`sentinel status` 子命令——列出所有感測現況表。

import (
	"fmt"
	"os"

	"slo-sentinel/config"
	"slo-sentinel/internal/store"
)

func runStatus(cfgPath, dbOverride string) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 設定載入失敗：%v\n", err)
		os.Exit(1)
	}
	dbPath := cfg.DBPath
	if dbOverride != "" {
		dbPath = dbOverride
	}
	st, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 開啟 %s 失敗：%v\n", dbPath, err)
		os.Exit(1)
	}
	defer st.Close()

	fmt.Println("感測 | 狀態 | 最後值 | 更新時間")
	fmt.Println("---|---|---|---")

	rows, err := st.AllStates()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 讀取狀態：%v\n", err)
		os.Exit(1)
	}
	for _, r := range rows {
		fmt.Printf("%s | %s | %.4g | %s\n", r.SensorID, r.State, r.LastValue, r.UpdatedAt.Format("2006-01-02 15:04"))
	}
}
