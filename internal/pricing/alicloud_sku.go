package pricing

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// 本檔實作阿里雲 BSSOpenAPI QuerySkuPriceList（algs/cost-forecast.md §D.0）：
// RPC 簽名（HMAC-SHA1 + POP 編碼），唯讀 RAM key 即可呼叫——非 billing 管理權。
// 端點預設 https://business.aliyuncs.com，Version 2017-12-14。

const defaultAliEndpoint = "https://business.aliyuncs.com"

// AlicloudSKU 為 QuerySkuPriceList 客戶端。
type AlicloudSKU struct {
	AccessKeyID     string
	AccessKeySecret string
	Endpoint        string // 測試可覆寫（fake server）
	Client          *http.Client

	nonce func() string // 測試注入用；nil 時用 UnixNano
}

// SkuPrice 為單一 SKU 的模組單價。
type SkuPrice struct {
	Code        string
	ModuleCode  string
	Price       float64
	Currency    string // 原幣別（CNY…）——§D.4 如實保留
	PricingUnit string
}

// QuerySkuPriceList 查詢指定 module/region/計費型態的 SKU 單價清單。
func (a *AlicloudSKU) QuerySkuPriceList(ctx context.Context, moduleCode, subscriptionType, regionID string) ([]SkuPrice, error) {
	if a.AccessKeyID == "" || a.AccessKeySecret == "" {
		return nil, ErrNoCredentials
	}
	params := map[string]string{
		"Action":           "QuerySkuPriceList",
		"AccessKeyId":      a.AccessKeyID,
		"Format":           "JSON",
		"Version":          "2017-12-14",
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureVersion": "1.0",
		"SignatureNonce":   a.nonceValue(),
		"Timestamp":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	if moduleCode != "" {
		params["ModuleCode"] = moduleCode
	}
	if subscriptionType != "" {
		params["SubscriptionType"] = subscriptionType
	}
	if regionID != "" {
		params["RegionId"] = regionID
	}

	signed := rpcSign(params, a.AccessKeySecret)
	q := url.Values{}
	for k, v := range signed {
		q.Set(k, v)
	}
	endpoint := a.Endpoint
	if endpoint == "" {
		endpoint = defaultAliEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint+"/?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	client := a.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pricing: bss querysku request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pricing: bss querysku 回應 %d: %s", resp.StatusCode, snippet(raw))
	}
	return parseQuerySkuPriceList(raw)
}

// parseQuerySkuPriceList 解析回應：
//
//	{"Data":{"Skus":{"Sku":[{"Code":…,"ModuleList":{"Module":[
//	    {"ModuleCode":…,"Price":"0.25","Currency":"CNY","PricingUnit":"…"}]}}]}}}
func parseQuerySkuPriceList(raw []byte) ([]SkuPrice, error) {
	var resp struct {
		Data struct {
			Skus struct {
				Sku []struct {
					Code       string `json:"Code"`
					ModuleList struct {
						Module []struct {
							ModuleCode  string  `json:"ModuleCode"`
							Price       float64 `json:"Price,string"`
							Currency    string  `json:"Currency"`
							PricingUnit string  `json:"PricingUnit"`
						} `json:"Module"`
					} `json:"ModuleList"`
				} `json:"Sku"`
			} `json:"Skus"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("pricing: 解析 queryskupricelist 回應: %w", err)
	}
	var out []SkuPrice
	for _, sku := range resp.Data.Skus.Sku {
		for _, m := range sku.ModuleList.Module {
			out = append(out, SkuPrice{
				Code: sku.Code, ModuleCode: m.ModuleCode,
				Price: m.Price, Currency: m.Currency, PricingUnit: m.PricingUnit,
			})
		}
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

func (a *AlicloudSKU) nonceValue() string {
	if a.nonce != nil {
		return a.nonce()
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// rpcSign 計算 Alibaba RPC 簽名（HMAC-SHA1，GET&%2F&<canonical>）並回傳含 Signature 的完整參數。
func rpcSign(params map[string]string, secret string) map[string]string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var pairs, canonical []string
	for _, k := range keys {
		v := params[k]
		pairs = append(pairs, k+"="+popEncode(v))
		canonical = append(canonical, popEncode(k)+"="+popEncode(v))
	}
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte("GET&%2F&" + popEncode(strings.Join(canonical, "&"))))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	pairs = append(pairs, "Signature="+popEncode(sig))

	out := map[string]string{}
	for _, p := range pairs {
		kv := strings.SplitN(p, "=", 2)
		out[kv[0]] = kv[1]
	}
	return out
}

// popEncode 為 Alibaba POP 特殊 URL 編碼慣例（+→%20 等）。
func popEncode(s string) string {
	var b strings.Builder
	for _, ch := range []byte(s) {
		switch {
		case (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') ||
			ch == '-' || ch == '_' || ch == '.' || ch == '~':
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}
