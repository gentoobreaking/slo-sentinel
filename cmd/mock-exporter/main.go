// mock-exporter——本機測試用的雲端月流量配額指標端點（非生產元件）。
//
// 提供 OpenMetrics 格式的模擬資料：
//
//	mock_hosts_count{...} N
//	mock_host_monthly_sent_bytes_total{host_id=...,traffic_type="public"} <遞增值>
//
// 語意模擬雲端供應商常見的「當月流量計費」counter：單調遞增、月初歸零。
// 數值從 -start 起、以 -rate 速率遞增，預設調校成「啟動後第一輪輪詢就能看到
// sentinel 判 critical」的演示節奏。
//
// 用法：
//
//	go run ./cmd/mock-exporter -listen :9999 \
//	  -start-bytes 42000000000000 -rate-bytes-per-hour 500000000000
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"
)

func main() {
	listen := flag.String("listen", ":9999", "監聽位址")
	start := flag.Float64("start-bytes", 42e12, "初始已用量（bytes）")
	rate := flag.Float64("rate-bytes-per-hour", 500e9, "成長速率（bytes/hour）")
	flag.Parse()

	startTime := time.Now()
	log.Printf("mock-exporter 啟動 %s｜起始 %.1fGB｜速率 %.0fGB/h",
		*listen, *start/1e9, *rate/1e9)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		elapsed := time.Since(startTime).Hours()
		sent := *start + *rate*elapsed
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, `# HELP mock_hosts_count Count of the hosts
# TYPE mock_hosts_count gauge
mock_hosts_count{host_type="dedicated_server",location_code="mock-dc"} 1

# HELP mock_host_monthly_sent_bytes_total Host monthly sent bytes total
# TYPE mock_host_monthly_sent_bytes_total counter
mock_host_monthly_sent_bytes_total{host_id="mock1",title="mock-host",traffic_type="public",location_code="mock-dc"} %.0f
mock_host_monthly_sent_bytes_total{host_id="mock1",title="mock-host",traffic_type="private",location_code="mock-dc"} %.0f
`, sent, sent*0.1)
	})

	log.Fatal(http.ListenAndServe(*listen, nil))
}
