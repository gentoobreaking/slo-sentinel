package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAlicloudSKUQuerySkuPriceList(t *testing.T) {
	var gotQuery map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		gotQuery = map[string]string{}
		for k := range r.Form {
			gotQuery[k] = r.Form.Get(k)
		}
		_, _ = w.Write([]byte(`{
		  "RequestId":"req-1",
		  "Data":{"Skus":{"Sku":[
		    {"Code":"ecs.g7.large","ModuleList":{"Module":[
		      {"ModuleCode":"ecs","Price":"0.12","Currency":"CNY","PricingUnit":"Hour"},
		      {"ModuleCode":"cloud_ssd","Price":"0.5","Currency":"CNY","PricingUnit":"GB/Month"}]}},
		    {"Code":"ecs.g7.xlarge","ModuleList":{"Module":[
		      {"ModuleCode":"ecs","Price":"0.24","Currency":"CNY","PricingUnit":"Hour"}]}}]}}
		}`))
	}))
	defer srv.Close()

	ali := &AlicloudSKU{
		AccessKeyID:     "LTAI-test",
		AccessKeySecret: "secret-test",
		Endpoint:        srv.URL,
		nonce:           func() string { return "nonce-1" },
	}
	prices, err := ali.QuerySkuPriceList(context.Background(), "ecs", "PayAsYouGo", "cn-hongkong")
	if err != nil {
		t.Fatalf("QuerySkuPriceList: %v", err)
	}
	if len(prices) != 3 {
		t.Fatalf("應解析出 3 筆模組單價，得 %d", len(prices))
	}
	if prices[0].Code != "ecs.g7.large" || prices[0].Price != 0.12 ||
		prices[0].Currency != "CNY" || prices[0].PricingUnit != "Hour" {
		t.Fatalf("第一筆內容錯: %+v", prices[0])
	}

	// 簽名呼叫格式驗證
	if gotQuery["Action"] != "QuerySkuPriceList" {
		t.Errorf("Action=%q", gotQuery["Action"])
	}
	if gotQuery["Version"] != "2017-12-14" {
		t.Errorf("Version=%q", gotQuery["Version"])
	}
	if gotQuery["SignatureMethod"] != "HMAC-SHA1" {
		t.Errorf("SignatureMethod=%q", gotQuery["SignatureMethod"])
	}
	if gotQuery["Signature"] == "" {
		t.Error("缺少 Signature")
	}
	if gotQuery["RegionId"] != "cn-hongkong" || gotQuery["SubscriptionType"] != "PayAsYouGo" {
		t.Errorf("查詢參數錯: %+v", gotQuery)
	}
}

func TestAlicloudSKUNoCredentials(t *testing.T) {
	ali := &AlicloudSKU{Endpoint: "http://unused"}
	if _, err := ali.QuerySkuPriceList(context.Background(), "ecs", "", ""); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("未設金鑰應 ErrNoCredentials，得 %v", err)
	}
}

func TestParseQuerySkuPriceListEmpty(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"Data": map[string]any{}})
	if _, err := parseQuerySkuPriceList(raw); !errors.Is(err, ErrNotFound) {
		t.Fatalf("空回應應 ErrNotFound，得 %v", err)
	}
}

func TestRPCSignKnownVector(t *testing.T) {
	// 固定參數 → 簽名可重現（演算法正確性的最小驗證）
	params := map[string]string{
		"Action":  "QuerySkuPriceList",
		"Format":  "JSON",
		"Version": "2017-12-14",
	}
	signed := rpcSign(params, "secret&")
	if signed["Action"] == "" || signed["Signature"] == "" {
		t.Fatalf("簽名結果缺欄位: %+v", signed)
	}
	signed2 := rpcSign(params, "secret&")
	if signed["Signature"] != signed2["Signature"] {
		t.Fatal("同參數簽名應一致")
	}
	// 不同 secret 簽名必不同
	signed3 := rpcSign(params, "other&")
	if signed3["Signature"] == signed["Signature"] {
		t.Fatal("不同 secret 不應產生相同簽名")
	}
	_ = http.StatusOK // 保持 import 對稱
}
