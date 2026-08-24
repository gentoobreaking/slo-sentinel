// sentinel daemon 進入點（T009）。
//
// 用法：
//
//	sentinel [-config path]        # daemon 模式（預設）
//	sentinel status [-config path] # 列出所有感測現況表
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"slo-sentinel/config"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "status" {
		fs := flag.NewFlagSet("status", flag.ExitOnError)
		cfgPath := fs.String("config", "", "設定檔路徑")
		dbPath := fs.String("db", "", "SQLite 路徑（覆寫 config）")
		fs.Parse(os.Args[2:])
		runStatus(*cfgPath, *dbPath)
		return
	}

	configPath := flag.String("config", "", "設定檔路徑（YAML）；留空使用預設值")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 設定載入失敗：%v\n", err)
		os.Exit(1)
	}
	setupLog(cfg.LogFormat)
	slog.Info("sentinel_started",
		"poll_interval_sec", cfg.PollIntervalSec,
		"prometheus_url", cfg.PrometheusURL,
		"alertmanager_url", cfg.AlertManagerURL,
	)
	d := newDaemon(cfg, slog.Default(), nil, nil) // src/store 由各任務注入；此處走 setupSensors 前的組裝
	if err := runDaemon(d, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
}

func setupLog(format string) {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	var h slog.Handler
	if format == "json" {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(h))
}

// runDaemon 組裝依賴並執行主迴圈。
func runDaemon(d *daemon, cfg config.Config) error {
	// T009：src 與 store 在此建立（gate 之外的元件皆為本地）
	src := newPrometheusSource(cfg.PrometheusURL)
	st, err := openStore(resolveDBPath(cfg.DBPath))
	if err != nil {
		return err
	}
	defer st.Close()

	d.src = src
	d.st = st
	ctx, cancel := contextWithSignal(context.Background())
	defer cancel()
	return d.Run(ctx)
}

// resolveDBPath 解析 SQLite 路徑：絕對路徑照用（容器部署必需）；
// 相對路徑維持原本「相對工作目錄」語意。
func resolveDBPath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(".", p)
}
