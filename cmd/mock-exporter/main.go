// mock-exporter——本機測試用的假 servers.com 指標端點（非生產元件）。
//
// 模擬 ListHostsMetrics 的 OpenMetrics 輸出：
//
//	serverscom_hosts_count{...} N
//	serverscom_host_monthly_sent_bytes_total{host_id=...,traffic_type="public"} <遞增值>
//
// 數值從 -start 起、以 -rate 的速率速率遞增，預設調校成「幾分鐘內就能看到
// sentinel 從 healthy → warning → critical」的演示節奏。
//
// 用法：
//
//	go run ./cmd/mock-exporter -listen :9999 \
//	  -start-bytes 42000000000000 -rate-bytes-per-hour 500000000000
//
// 對應真實環境：把 Prometheus scrape target 換成 api.servers.com＋真 token 即可，
// capacity_def 不需改動（label 相同）。
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
		fmt.Fprintf(w, `# HELP serverscom_hosts_count Count of the hosts
# TYPE serverscom_hosts_count gauge
serverscom_hosts_count{host_type="dedicated_server",location_code="mock-dc"} 1

# HELP serverscom_host_monthly_sent_bytes_total Host monthly sent bytes total
# TYPE serverscom_host_monthly_sent_bytes_total counter
serverscom_host_monthly_sent_bytes_total{host_id="mock1",title="mock-host",traffic_type="public",location_code="mock-dc"} %.0f
serverscom_host_monthly_sent_bytes_total{host_id="mock1",title="mock-host",traffic_type="private",location_code="mock-dc"} %.0f
`, sent, sent*0.1)
	})

	log.Fatal(http.ListenAndServe(*listen, nil))
}
