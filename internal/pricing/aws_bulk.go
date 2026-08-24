package pricing

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// 本檔實作 AWS Price List Bulk API 的離線解析：
//   1. LocateOfferURL：解析 index.json（小檔），定位 service+region 的 offer 檔 URL
//   2. FindOnDemandRates：串流解析 offer 檔（大檔，禁止一次性載入——json.Decoder 逐步處理）
//
// offer 檔結構（AWS Price List Query API 與 Bulk 檔共用 terms 結構）：
//
//	{ "products": { "<SKU>": {"sku":…, "productFamily":…, "attributes":{…}} },
//	  "terms":    { "OnDemand": { "<SKU>": { "<termID>": {
//	      "priceDimensions": { "<dimID>": {"unit":"Hrs", "pricePerUnit":{"USD":"0.096"}} }}}}} }

// indexOffer 為 index.json 中單一服務的定位資訊。
type indexOffer struct {
	OfferCode             string            `json:"offerCode"`
	CurrentRegionIndexURL string            `json:"currentRegionIndexUrl"`
	Regions               map[string]string `json:"regions"` // region → 該 region offer 檔 URL
}

// LocateOfferURL 解析 index.json，回傳 serviceCode+region 對應的 offer 檔 URL。
// 若該 region 未列於 regions，回退 currentRegionIndexUrl（region 索引檔）。
func LocateOfferURL(index []byte, serviceCode, region string) (string, error) {
	var idx struct {
		Offers map[string]indexOffer `json:"offers"`
	}
	if err := json.Unmarshal(index, &idx); err != nil {
		return "", fmt.Errorf("pricing: 解析 index.json: %w", err)
	}
	off, ok := idx.Offers[serviceCode]
	if !ok {
		return "", fmt.Errorf("pricing: index.json 無服務 %q: %w", serviceCode, ErrNotFound)
	}
	if u := off.Regions[region]; u != "" {
		return u, nil
	}
	if off.CurrentRegionIndexURL != "" {
		return off.CurrentRegionIndexURL, nil
	}
	return "", fmt.Errorf("pricing: 服務 %q 找不到 region %q 的 offer 檔", serviceCode, region)
}

// ProductMatcher 回傳 true 表示該 SKU 符合查詢條件。
// family 為價目表的 productFamily（頂層欄位，非 attributes 成員）；attrs 原樣來自價目表。
type ProductMatcher func(sku, family string, attrs map[string]string) bool

// FindOptions 控制串流解析的資源上限。
type FindOptions struct {
	MaxResults int // 最多保留的費率筆數——記憶體峰值上限的關鍵參數（≤0 視為 1）
}

// Rate 為單一 OnDemand 費率。
type Rate struct {
	SKU          string
	Unit         string
	Currency     string
	PricePerUnit float64
}

// odTerm 對應 terms.OnDemand.<SKU>.<offerTermCode>，只取需要的欄位。
type odTerm struct {
	OfferTermCode   string `json:"offerTermCode"`
	PriceDimensions map[string]struct {
		Unit         string            `json:"unit"`
		Description  string            `json:"description"`
		PricePerUnit map[string]string `json:"pricePerUnit"`
	} `json:"priceDimensions"`
}

// firstRate 自一個 term 取出第一個（依 dimension ID 排序）有 USD 以外幣別也照收的費率，
// 幣別如實回傳（§D.4）。
func firstRate(sku string, t odTerm) (Rate, bool) {
	keys := make([]string, 0, len(t.PriceDimensions))
	for k := range t.PriceDimensions {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		dim := t.PriceDimensions[k]
		for cur, priceStr := range dim.PricePerUnit {
			v, err := parsePrice(priceStr)
			if err != nil || v <= 0 {
				continue
			}
			return Rate{SKU: sku, Unit: dim.Unit, Currency: cur, PricePerUnit: v}, true
		}
	}
	return Rate{}, false
}

// productEntry 只保留 attributes；sku/productFamily 供 matcher 判斷。
type productEntry struct {
	SKU           string            `json:"sku"`
	ProductFamily string            `json:"productFamily"`
	Attributes    map[string]string `json:"attributes"`
}

// FindOnDemandRates 以 json.Decoder 逐步處理 offer 檔串流：
// products 區段逐 SKU 解碼、不符合即丟棄（不累積）；terms 只解碼符合條件的 SKU。
// 記憶體峰值受 MaxResults 上限與固定大小的讀取緩衝約束。
func FindOnDemandRates(r io.Reader, match ProductMatcher, opts FindOptions) ([]Rate, error) {
	max := opts.MaxResults
	if max <= 0 {
		max = 1
	}
	br := bufio.NewReaderSize(r, 64<<10) // 固定 64KB 讀取緩衝——峰值上限的一部分
	dec := json.NewDecoder(br)

	matched := make(map[string]bool)
	var rates []Rate

	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("pricing: offer 檔非 JSON 物件: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("pricing: offer 檔頂層不是物件")
	}

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch key := keyTok.(string); key {
		case "products":
			if err := scanProducts(dec, match, matched, max); err != nil {
				return nil, err
			}
		case "terms":
			rates, err = scanTerms(dec, matched, max)
			if err != nil {
				return nil, err
			}
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, err
			}
		}
	}
	if rates == nil {
		return nil, ErrNotFound
	}
	return rates, nil
}

// scanProducts 逐一解碼 products.<SKU>；不符合者 Decode 完即丟棄，不保留。
func scanProducts(dec *json.Decoder, match ProductMatcher, matched map[string]bool, max int) error {
	if _, err := dec.Token(); err != nil { // '{'
		return err
	}
	for dec.More() {
		skuTok, err := dec.Token()
		if err != nil {
			return err
		}
		var p productEntry
		if err := dec.Decode(&p); err != nil {
			return fmt.Errorf("pricing: 解析 product %v: %w", skuTok, err)
		}
		sku := p.SKU
		if sku == "" {
			sku, _ = skuTok.(string)
		}
		if len(matched) < max && match(sku, p.ProductFamily, p.Attributes) {
			matched[sku] = true
		}
	}
	_, err := dec.Token() // '}'
	return err
}

// scanTerms 逐區段處理 terms；只有 OnDemand 有興趣，其餘（Reserved…）
// 以空結構體解碼跳過——解析但不累積，維持峰值上限。
func scanTerms(dec *json.Decoder, matched map[string]bool, max int) ([]Rate, error) {
	if _, err := dec.Token(); err != nil { // '{'
		return nil, err
	}
	defer func() { dec.Token() }() // '}'

	var rates []Rate
	have := false
	for dec.More() {
		kindTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		kind, _ := kindTok.(string)
		if _, err := dec.Token(); err != nil { // '{' of <kind>
			return nil, err
		}
		if kind != "OnDemand" {
			var skip struct{}
			if err := dec.Decode(&skip); err != nil {
				return nil, err
			}
			continue
		}
		for dec.More() {
			skuTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			sku, _ := skuTok.(string)
			if !matched[sku] || have && len(rates) >= max {
				var skip struct{} // 解析但丟棄，不保留位元組
				if err := dec.Decode(&skip); err != nil {
					return nil, fmt.Errorf("pricing: 跳過 terms.%s.%s: %w", kind, sku, err)
				}
				continue
			}
			var termMap map[string]odTerm
			if err := dec.Decode(&termMap); err != nil {
				return nil, fmt.Errorf("pricing: 解析 OnDemand %s: %w", sku, err)
			}
			for termID, t := range termMap {
				_ = termID
				if r, ok := firstRate(sku, t); ok {
					rates = append(rates, r)
					have = true
					break // 每 SKU 一筆即可
				}
			}
		}
		if _, err := dec.Token(); err != nil { // '}' of <kind>
			return nil, err
		}
	}
	return rates, nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
