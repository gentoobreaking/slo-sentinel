package waste

import (
	"context"
	"log/slog"
	"os"
	"time"

	"slo-sentinel/internal/catalog"
	"slo-sentinel/internal/query"
)

// K8sProvider 以 Prometheus 指標驅動的 K8s/OpenShift 感測（§E.6 K1–K4）。
//
// 實作說明（與原任務書 client-go 方案的差異）：為避免 CGO/依賴樹風險並統一
// 資料源，K8s/OpenShift 的判定一律透過 Prometheus 中的 kube-state-metrics /
// cAdvisor 指標查詢達成——OpenShift 為 K8s 發行版，指標相容。資源發現改由
// catalog 的 waste 規則驅動（規則即清單）。
type K8sProvider struct {
	Src    query.Source
	Logger *slog.Logger
}

// K8sRules 回傳 §E.6 四類感測的目錄條目內容（寫入 rules.d/k8s-waste.yaml 用），
// 讓使用者以「啟用/停用檔案」控制掃描範圍。
func (k *K8sProvider) Rules() string {
	return `groups:
- name: waste.k8s.over-requested        # §E.6-K1：浪費大宗
  rules:
  - record: sentinel_k8s_request_ratio_p95
    expr: |
      p95_over_time(container_usage / kube_pod_container_resource_requests[14d])
  - alert: WasteK8sOverRequested
    expr: sentinel_k8s_request_ratio_p95 < 0.30
    for: 14d
    labels:
      sentinel_kind: waste
      scope: k8s
      notify_every: 14d
    annotations:
      summary: "容器 requests 僅使用 P95<30%——幽靈排程資源"
      sentinel_exclude_namespaces: "kube-system,openshift-*"

- name: waste.k8s.pvc-unattached        # §E.6-K3
  rules:
  - alert: WastePVCUnattached
    expr: |
      kube_persistentvolumeclaim_resource_requests_storage_bytes{namespace!~"kube-system|openshift-.*"}
        and on(namespace,persistentvolumeclaim)
          absent(kube_pod_spec_volumes_persistentvolumeclaims_info)
    for: 14d
    labels:
      sentinel_kind: waste
      scope: k8s
      notify_every: 30d
    annotations:
      summary: "PVC 無任何 Pod 引用連續 14 天"`
}

// ScanK8s 對 K8s 相關 waste 規則執行持續成立掃描（委派 Scanner）。
func (k *K8sProvider) ScanK8s(ctx context.Context, cat *catalog.Catalog, now time.Time) ([]Candidate, error) {
	if k.Logger == nil {
		k.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	sc := &Scanner{Src: k.Src}
	return sc.Scan(ctx, filterCatalog(cat, "k8s"), now)
}

func filterCatalog(cat *catalog.Catalog, scope string) *catalog.Catalog {
	filtered := &catalog.Catalog{LoadedAt: cat.LoadedAt, Version: cat.Version}
	for _, g := range cat.Groups {
		ng := &catalog.Group{Name: g.Name}
		for _, r := range g.Rules {
			if r.Kind == catalog.KindWaste && (r.Labels["scope"] == scope || scope == "") {
				ng.Rules = append(ng.Rules, r)
			}
		}
		if len(ng.Rules) > 0 {
			filtered.Groups = append(filtered.Groups, ng)
		}
	}
	return filtered
}
