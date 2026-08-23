// sentinel daemon 進入點（T001 骨架；T009 接上主輪詢迴圈）。
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"slo-sentinel/config"
)

func main() {
	configPath := flag.String("config", "", "設定檔路徑（YAML）；留空使用預設值")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 設定載入失敗：%v\n", err)
		os.Exit(1)
	}

	level := slog.LevelInfo
	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(handler))

	slog.Info("sentinel_started",
		"poll_interval_sec", cfg.PollIntervalSec,
		"prometheus_url", cfg.PrometheusURL,
		"alertmanager_url", cfg.AlertManagerURL,
	)
	// T009：主輪詢迴圈將於此接上
}
