package billing

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

// AlicloudBSS 以 BSS OpenAPI（business.aliyuncs.com，RPC 簽名）實作 BillingSource。
// 資料延遲 ~24h（§D.1）。
type AlicloudBSS struct {
	AccessKeyID     string
	AccessKeySecret string
	Endpoint        string // 預設 https://business.aliyuncs.com
	Client          *http.Client
}

func (a *AlicloudBSS) Name() string { return "alicloud-bss" }

// DailySpend 呼叫 DescribeInstanceBill（Granularity=DAILY），
// 彙整為每日花費。v1：以 InstanceID 為「服務」近似值。
func (a *AlicloudBSS) DailySpend(ctx context.Context, f Filter, start, end time.Time) ([]DailySpend, error) {
	params := map[string]string{
		"Action":           "DescribeInstanceBill",
		"AccessKeyId":      a.AccessKeyID,
		"Format":           "JSON",
		"Version":          "2017-12-14",
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureVersion": "1.0",
		"SignatureNonce":   fmt.Sprintf("%d", time.Now().UnixNano()),
		"Timestamp":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"BillingCycle":     start.Format("2006-01"),
		"Granularity":      "DAILY",
	}
	for k, v := range f.Tags {
		params["TagKey_"+k] = v // v1 簡化；正式標籤過濾以 TagResources 對接為 v2
	}

	signed := a.signParams(params)
	q := url.Values{}
	keys := make([]string, 0, len(signed))
	for k := range signed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		q.Set(k, signed[k])
	}

	endpoint := a.Endpoint
	if endpoint == "" {
		endpoint = "https://business.aliyuncs.com"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint+"/?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	client := a.Client
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bss request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bss 回應 %d: %s", resp.StatusCode, truncateBytes(raw, 300))
	}

	var bssResp struct {
		Data struct {
			Items struct { // BSS 回應：Items 為物件內含 Item 陣列（非標準 JSON 慣例）
				Item []struct {
					Date string  `json:"UsageStartDate"`
					Cost float64 `json:"Cost"` // v1 未分幣別，以回傳值為準
				} `json:"Item"`
			} `json:"Items"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(raw, &bssResp); err != nil {
		return nil, fmt.Errorf("decode bss: %w", err)
	}

	var out []DailySpend
	for _, item := range bssResp.Data.Items.Item {
		date, err := time.Parse("2006-01-02", item.Date)
		if err != nil {
			continue
		}
		out = append(out, DailySpend{Date: date, CostUSD: item.Cost})
	}
	return out, nil
}

// signParams 計算 Alibaba RPC 簽名（HMAC-SHA1 + POP 編碼慣例）並回傳含 Signature 的完整參數。
func (a *AlicloudBSS) signParams(params map[string]string) map[string]string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var pairs []string
	var canonicalPairs []string
	for _, k := range keys {
		v := params[k]
		pairs = append(pairs, k+"="+esc(v))
		canonicalPairs = append(canonicalPairs, esc(k)+"="+esc(v))
	}
	stringToSign := "GET&%2F&" + esc(strings.Join(canonicalPairs, "&"))
	mac := hmac.New(sha1.New, []byte(a.AccessKeySecret+"&"))
	mac.Write([]byte(stringToSign))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	pairs = append(pairs, "Signature="+esc(sig))

	out := map[string]string{}
	for _, p := range pairs {
		kv := strings.SplitN(p, "=", 2)
		out[kv[0]] = kv[1]
	}
	_ = url.Values{}.Encode()
	return out
}

func esc(s string) string {
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
