package pricing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// httptest fake：驗證無認證請求格式（serviceCode/filters/TERM_MATCH）與回應解析。
func TestQueryClientGetProducts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("應為 POST，得 %s", r.Method)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("Price List Query API 免認證，不應有 Authorization header")
		}
		var body struct {
			ServiceCode string      `json:"serviceCode"`
			Filters     []queryWire `json:"filters"`
			NextToken   string      `json:"nextToken"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("解碼請求: %v", err)
		}
		if body.ServiceCode != "AmazonEC2" {
			t.Errorf("serviceCode=%q", body.ServiceCode)
		}
		if len(body.Filters) == 0 || body.Filters[0].Type != "TERM_MATCH" {
			t.Fatalf("filters 格式錯: %+v", body.Filters)
		}

		product := `{"product":{"sku":"ABCD1234","productFamily":"Compute Instance",
		  "attributes":{"instanceType":"m5.large","regionCode":"us-east-1"}},
		  "terms":{"OnDemand":{"ABCD1234.T1":{"offerTermCode":"T1","priceDimensions":{
		    "ABCD1234.T1.D1":{"unit":"Hrs","pricePerUnit":{"USD":"0.096"}}}}}}}`
		resp := map[string]any{"FormatVersion": "aws_price_list_query_api_v1"}
		if body.NextToken == "" {
			resp["PriceList"] = []string{product}
			resp["NextToken"] = "PAGE2"
		} else { // 分頁：第二頁後結束
			resp["PriceList"] = []string{product}
			resp["NextToken"] = ""
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	q := &QueryClient{Endpoint: srv.URL + "/"}
	products, err := q.GetProducts(context.Background(), "AmazonEC2", []QueryFilter{
		{Field: "regionCode", Value: "us-east-1"},
	})
	if err != nil {
		t.Fatalf("GetProducts: %v", err)
	}
	if len(products) != 2 {
		t.Fatalf("分頁應合併 2 筆，得 %d", len(products))
	}
	r, ok := rateOf(products[0])
	if !ok || r.PricePerUnit != 0.096 || r.Currency != "USD" || r.Unit != "Hrs" {
		t.Fatalf("費率解析錯: %+v ok=%v", r, ok)
	}
}

func TestQueryClientErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`upstream error`))
	}))
	defer srv.Close()
	q := &QueryClient{Endpoint: srv.URL + "/"}
	if _, err := q.GetProducts(context.Background(), "AmazonEC2", nil); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("應回傳含狀態碼錯誤，得 %v", err)
	}
}

// parsePrice 邊界。
func TestParsePrice(t *testing.T) {
	v, err := parsePrice(" 0.096 ")
	if err != nil || v != 0.096 {
		t.Fatalf("parsePrice: %v %v", v, err)
	}
}
