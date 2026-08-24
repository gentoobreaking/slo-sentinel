package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Catalog 為「用量指標 → 單價」查詢門面（對接 cost 引擎 estimate 模式，§D.0）。
//
// 查詢策略：
//  1. 快取命中（TTL 內）直接回傳——AWS 官方每日多次更新、阿里雲建議每日一次
//  2. AWS 家族走 Price List Query API（免認證、輕量首選）
//  3. 阿里雲家族走 QuerySkuPriceList（需唯讀 RAM key；未設金鑰回 ErrNoCredentials）
//  4. 全部失敗時，若快取存在過期內容則使用並標注 Stale=true
type Catalog struct {
	TTL      time.Duration // 快取新鮮期；預設 24h
	CacheDir string        // 快取目錄；空字串＝停用

	AWSQuery *QueryClient // nil 時用預設
	Ali      *AlicloudSKU

	client *http.Client
}

// Quote 回傳單價報價（含幣別與計價單位）。
func (c *Catalog) Quote(ctx context.Context, f Family, attrs Attrs) (Quote, error) {
	ttl := c.TTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	cache := &FileCache{Dir: c.CacheDir}
	key := CacheKey(f, attrs)

	if hit := cache.Load(key, ttl); hit.OK && hit.Fresh {
		var q Quote
		if err := json.Unmarshal(hit.Payload, &q); err == nil {
			return q, nil
		}
	}

	q, err := c.resolve(ctx, f, attrs)
	if err != nil {
		if hit := cache.Load(key, ttl); hit.OK { // 離線 fallback：過期快取＋標注 stale
			var stale Quote
			if jerr := json.Unmarshal(hit.Payload, &stale); jerr == nil {
				stale.Stale = true
				return stale, nil
			}
		}
		return Quote{}, err
	}

	if raw, jerr := json.Marshal(q); jerr == nil {
		_ = cache.Store(key, raw)
	}
	return q, nil
}

// Price 為任務書定義的查詢介面：Price(family, attrs) (unitPrice, currency, error)。
func (c *Catalog) Price(ctx context.Context, f Family, attrs Attrs) (unitPrice float64, currency string, err error) {
	q, err := c.Quote(ctx, f, attrs)
	if err != nil {
		return 0, "", err
	}
	return q.UnitPrice, q.Currency, nil
}

func (c *Catalog) resolve(ctx context.Context, f Family, attrs Attrs) (Quote, error) {
	switch attrs.get("cloud", "aws") {
	case "alicloud":
		return c.resolveAli(ctx, f, attrs)
	default:
		return c.resolveAWS(ctx, f, attrs)
	}
}

// ---- AWS：Query API 為主，v1 固定三個家族 ----

func (c *Catalog) queryClient() *QueryClient {
	if c.AWSQuery != nil {
		return c.AWSQuery
	}
	return &QueryClient{}
}

func (c *Catalog) resolveAWS(ctx context.Context, f Family, attrs Attrs) (Quote, error) {
	region := attrs.get("region", "us-east-1")
	switch f {
	case FamEC2:
		return c.ec2Quote(ctx, region, attrs)
	case FamEBS:
		return c.storageQuote(ctx, "AmazonEC2", region, []QueryFilter{
			{Field: "volumeType", Value: attrs.get("volume_type", "gp3")},
			{Field: "productFamily", Value: "Storage"},
		}, "GB-Mo")
	case FamRDS:
		filters := []QueryFilter{
			{Field: "productFamily", Value: "Database Storage"},
		}
		if eng := attrs["engine"]; eng != "" {
			filters = append(filters, QueryFilter{Field: "databaseEngine", Value: eng})
		}
		if vt := attrs["volume_type"]; vt != "" {
			filters = append(filters, QueryFilter{Field: "volumeType", Value: vt})
		}
		return c.storageQuote(ctx, "AmazonRDS", region, filters, "GB-Mo")
	default:
		return Quote{}, fmt.Errorf("pricing: v1 未支援家族 %q（EC2/EBS/RDS）", f)
	}
}

func (c *Catalog) ec2Quote(ctx context.Context, region string, attrs Attrs) (Quote, error) {
	instanceType := attrs["instance_type"]
	if instanceType == "" {
		return Quote{}, fmt.Errorf("pricing: ec2 需要 instance_type 屬性")
	}
	osName := attrs.get("os", "Linux")
	products, err := c.queryClient().GetProducts(ctx, "AmazonEC2", []QueryFilter{
		{Field: "regionCode", Value: region},
		{Field: "instanceType", Value: instanceType},
		{Field: "operatingSystem", Value: osName},
		{Field: "capacitystatus", Value: "Used"},
		{Field: "preInstalledSw", Value: "NA"},
	})
	if err == nil {
		for _, p := range products {
			if p.Product.ProductFamily != "Compute Instance" {
				continue
			}
			if r, ok := rateOf(p); ok {
				return Quote{UnitPrice: r.PricePerUnit, Currency: r.Currency, Unit: r.Unit, Source: "aws-query"}, nil
			}
		}
		if len(products) > 0 || ctx.Err() == nil && products != nil {
			return Quote{}, fmt.Errorf("pricing: ec2 %s@%s: %w", instanceType, region, ErrNotFound)
		}
	}
	// Query API 失敗時不強制 fallback bulk（大檔下載是重量級操作）；
	// bulk 路徑保留給離線測試與明確指定 source=bulk 的呼叫端。
	if attrs["source"] != "bulk" {
		return Quote{}, fmt.Errorf("pricing: ec2 %s@%s 查詢失敗: %w", instanceType, region, err)
	}
	return c.ec2Bulk(ctx, region, instanceType, osName)
}

// ec2Bulk 以 Bulk API（index.json → offer 檔串流解析）取價。
func (c *Catalog) ec2Bulk(ctx context.Context, region, instanceType, osName string) (Quote, error) {
	hc := c.client
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Get(indexURL())
	if err != nil {
		return Quote{}, fmt.Errorf("pricing: 下載 index.json: %w", err)
	}
	indexRaw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20)) // index.json 為小檔，可整讀
	resp.Body.Close()
	if err != nil {
		return Quote{}, err
	}
	offerURL, err := LocateOfferURL(indexRaw, "AmazonEC2", region)
	if err != nil {
		return Quote{}, err
	}
	oResp, err := hc.Get(offerURL)
	if err != nil {
		return Quote{}, fmt.Errorf("pricing: 下載 offer 檔: %w", err)
	}
	defer oResp.Body.Close()

	rates, err := FindOnDemandRates(oResp.Body, func(sku, family string, a map[string]string) bool {
		return family == "Compute Instance" &&
			a["instanceType"] == instanceType &&
			a["operatingSystem"] == osName &&
			a["capacitystatus"] == "Used" &&
			a["preInstalledSw"] == "NA"
	}, FindOptions{MaxResults: 5})
	if err != nil {
		return Quote{}, err
	}
	r := rates[0]
	return Quote{UnitPrice: r.PricePerUnit, Currency: r.Currency, Unit: r.Unit, Source: "aws-bulk"}, nil
}

func (c *Catalog) storageQuote(ctx context.Context, serviceCode, region string, extra []QueryFilter, unit string) (Quote, error) {
	filters := append([]QueryFilter{{Field: "regionCode", Value: region}}, extra...)
	products, err := c.queryClient().GetProducts(ctx, serviceCode, filters)
	if err != nil {
		return Quote{}, fmt.Errorf("pricing: %s 儲存查詢: %w", serviceCode, err)
	}
	for _, p := range products {
		if r, ok := rateOf(p); ok {
			return Quote{UnitPrice: r.PricePerUnit, Currency: r.Currency, Unit: r.Unit, Source: "aws-query"}, nil
		}
	}
	return Quote{}, fmt.Errorf("pricing: %s 儲存 @%s: %w", serviceCode, region, ErrNotFound)
}

// ---- 阿里雲：QuerySkuPriceList（唯讀 RAM key）----

func (c *Catalog) resolveAli(ctx context.Context, f Family, attrs Attrs) (Quote, error) {
	ali := c.Ali
	if ali == nil {
		return Quote{}, ErrNoCredentials
	}
	subType := attrs.get("subscription_type", "PayAsYouGo")
	prices, err := ali.QuerySkuPriceList(ctx, string(f), subType, attrs["region"])
	if err != nil {
		return Quote{}, fmt.Errorf("pricing: alicloud %s 查詢: %w", f, err)
	}
	p := prices[0]
	return Quote{UnitPrice: p.Price, Currency: p.Currency, Unit: p.PricingUnit, Source: "alicloud-sku"}, nil
}

// ---- 測試掛點 ----

// indexURL 回傳 AWS index.json 位址（變數便於測試覆寫為 httptest URL）。
var indexURL = func() string {
	return "https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/index.json"
}
