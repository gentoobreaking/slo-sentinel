package pricing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// 本檔實作 AWS Price List Query API（免認證，輕量首選）：
// POST https://pricing.us-east-1.amazonaws.com/ 帶 JSON body
// {"serviceCode":…, "filters":[{"Field":…,"Type":"TERM_MATCH","Value":…}], "NextToken":…}
// 回應 {"FormatVersion":"aws_price_list_query_api_v1", "PriceList":["<JSON 字串>",…], "NextToken":…}

const defaultAWSQueryEndpoint = "https://pricing.us-east-1.amazonaws.com/"

// QueryFilter 為 TERM_MATCH 過濾條件。
type QueryFilter struct{ Field, Value string }

// QueryClient 為 Price List Query API 客戶端（無需任何認證）。
type QueryClient struct {
	Endpoint string       // 預設 https://pricing.us-east-1.amazonaws.com/（測試可覆寫）
	Client   *http.Client // nil 時用 http.DefaultClient
}

// queryProduct 對應 PriceList 陣列中每個內嵌 JSON 字串的結構。
type queryProduct struct {
	Product struct {
		SKU           string            `json:"sku"`
		ProductFamily string            `json:"productFamily"`
		Attributes    map[string]string `json:"attributes"`
	} `json:"product"`
	Terms struct {
		OnDemand map[string]odTerm `json:"OnDemand"`
	} `json:"terms"`
}

// GetProducts 分頁抓取符合過濾條件的產品＋OnDemand 費率。
func (q *QueryClient) GetProducts(ctx context.Context, serviceCode string, filters []QueryFilter) ([]queryProduct, error) {
	endpoint := q.Endpoint
	if endpoint == "" {
		endpoint = defaultAWSQueryEndpoint
	}
	client := q.Client
	if client == nil {
		client = http.DefaultClient
	}

	var out []queryProduct
	next := ""
	for page := 0; ; page++ {
		body := struct {
			ServiceCode string      `json:"serviceCode"`
			Filters     []queryWire `json:"filters"`
			MaxResults  int         `json:"maxResults,omitempty"`
			NextToken   string      `json:"nextToken,omitempty"`
		}{ServiceCode: serviceCode, MaxResults: 100}
		for _, f := range filters {
			body.Filters = append(body.Filters, queryWire{Field: f.Field, Type: "TERM_MATCH", Value: f.Value})
		}
		if next != "" {
			body.NextToken = next
		}
		raw, _ := json.Marshal(body)

		req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("pricing: query api request: %w", err)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("pricing: query api 回應 %d: %s", resp.StatusCode, snippet(data))
		}

		var parsed struct {
			PriceList []string `json:"PriceList"`
			NextToken string   `json:"NextToken"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil {
			return nil, fmt.Errorf("pricing: 解析 query api 回應: %w", err)
		}
		for _, elem := range parsed.PriceList {
			var p queryProduct
			if err := json.Unmarshal([]byte(elem), &p); err != nil {
				return nil, fmt.Errorf("pricing: 解析 PriceList 元素: %w", err)
			}
			out = append(out, p)
		}
		if parsed.NextToken == "" || page > 100 { // 分頁上限防呆
			return out, nil
		}
		next = parsed.NextToken
	}
}

type queryWire struct {
	Field string `json:"Field"`
	Type  string `json:"Type"`
	Value string `json:"Value"`
}

func snippet(b []byte) string {
	s := string(b)
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// rateOf 取出 product 的第一筆 OnDemand 費率。
func rateOf(p queryProduct) (Rate, bool) {
	for termID, t := range p.Terms.OnDemand {
		_ = termID
		return firstRate(p.Product.SKU, t)
	}
	return Rate{}, false
}

func parsePrice(s string) (float64, error) {
	return strconv.ParseFloat(strings.TrimSpace(s), 64)
}
