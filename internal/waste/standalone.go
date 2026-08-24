package waste

// standalone.go（T014）：Standalone server 感測（§E.7 S1–S3）。
//
// 資料源：node_exporter 指標（經 Prometheus）＋ conntrack/audit 判定。
// 以 catalog 的 waste 規則驅動，與 K8s provider 同一掃描架構。

import (
	"context"
	"time"

	"slo-sentinel/internal/catalog"
	"slo-sentinel/internal/query"
)

type StandaloneProvider struct {
	Src query.Source
}

// Rules 回傳 §E.7 三類感測的目錄條目內容。
func (p *StandaloneProvider) Rules() string {
	return `groups:
- name: waste.onprem.server             # §E.7-S1：機器級殭屍
  rules:
  - alert: WasteServerZombie
    expr: |
      p95_over_time(cpu_usage[14d]) < 10
        and p95_over_time(mem_usage[14d]) < 30
        and external_connections_14d == 0
    for: 14d
    labels:
      sentinel_kind: waste
      scope: onprem
      notify_every: 14d
    annotations:
      summary: "疑似殭屍主機（CPU/記憶體/連線皆閒置 14 天）"
      sentinel_sensor: onprem.server.zombie

- name: waste.onprem.ghost-service      # §E.7-S2：幽靈服務
  rules:
  - alert: WasteGhostService
    expr: |
      service_listening{state="listen"} unless on(instance,port) service_connections_active
    for: 14d
    labels:
      sentinel_kind: waste
      scope: onprem
      notify_every: 30d
    annotations:
      summary: "監聽埠 14 天內零外部連線"
      sentinel_sensor: onprem.service.ghost

- name: waste.onprem.disk-orphan        # §E.7-S3：磁碟孤兒
  rules:
  - alert: WasteDiskOrphan
    expr: |
      disk_growth_30d == 0 and disk_writes_30d == 0
    for: 30d
    labels:
      sentinel_kind: waste
      scope: onprem
      notify_every: 30d
    annotations:
      summary: "掛載點 30 天無成長且無程序寫入"
      sentinel_sensor: onprem.disk.orphan`
}

// ScanStandalone 對 standalone 相關規則執行持續成立掃描。
func (p *StandaloneProvider) ScanStandalone(ctx context.Context, cat *catalog.Catalog, now time.Time) ([]Candidate, error) {
	sc := &Scanner{Src: p.Src}
	return sc.Scan(ctx, filterCatalog(cat, "onprem"), now)
}
