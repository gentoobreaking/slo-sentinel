package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileCacheTTLAndStale(t *testing.T) {
	dir := t.TempDir()
	c := &FileCache{Dir: dir}

	if c.Load("k", time.Hour).OK {
		t.Fatal("空快取不應命中")
	}
	if err := c.Store("k", json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatalf("Store: %v", err)
	}
	hit := c.Load("k", time.Hour)
	if !hit.OK || !hit.Fresh || string(hit.Payload) != `{"a":1}` {
		t.Fatalf("TTL 內應新鮮命中: %+v", hit)
	}

	// 過期：Fresh=false 但內容仍在——離線 fallback 依據
	stale := (&FileCache{Dir: dir}).Load("k", -time.Second)
	if !stale.OK || stale.Fresh {
		t.Fatalf("過期快取應 OK=true Fresh=false: %+v", stale)
	}
	if string(stale.Payload) != `{"a":1}` {
		t.Fatalf("過期內容應保留: %s", stale.Payload)
	}
}

func TestCatalogPriceQueryAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		product := `{"product":{"sku":"S1","productFamily":"Compute Instance",
		  "attributes":{"instanceType":"m5.large"}},
		  "terms":{"OnDemand":{"S1.T":{"priceDimensions":{"S1.T.D":{"unit":"Hrs","pricePerUnit":{"USD":"0.096"}}}}}}}`
		_, _ = w.Write([]byte(`{"FormatVersion":"aws_price_list_query_api_v1","PriceList":[` +
			jsonQuote(product) + `],"NextToken":""}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	c := &Catalog{
		CacheDir: dir,
		TTL:      time.Hour,
		AWSQuery: &QueryClient{Endpoint: srv.URL + "/"},
	}
	price, cur, err := c.Price(context.Background(), FamEC2, Attrs{"region": "us-east-1", "instance_type": "m5.large"})
	if err != nil || price != 0.096 || cur != "USD" {
		t.Fatalf("Price: price=%v currency=%q err=%v", price, cur, err)
	}
}

// TTL 內第二次查詢不發出請求（快取命中）；TTL 過期且來源掛掉 → 使用過期快取＋Stale。
func TestCatalogCacheHitAndStaleFallback(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		product := `{"product":{"sku":"S1","productFamily":"Storage",
		  "attributes":{"volumeType":"gp3"}},
		  "terms":{"OnDemand":{"S1.T":{"priceDimensions":{"S1.T.D":{"unit":"GB-Mo","pricePerUnit":{"USD":"0.08"}}}}}}}`
		_, _ = w.Write([]byte(`{"PriceList":[` + jsonQuote(product) + `]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	c := &Catalog{
		CacheDir: dir,
		TTL:      time.Hour,
		AWSQuery: &QueryClient{Endpoint: srv.URL + "/"},
	}
	attrs := Attrs{"region": "us-east-1", "volume_type": "gp3"}

	q1, err := c.Quote(context.Background(), FamEBS, attrs)
	if err != nil || q1.UnitPrice != 0.08 {
		t.Fatalf("第一次查詢: %+v err=%v", q1, err)
	}
	q2, err := c.Quote(context.Background(), FamEBS, attrs)
	if err != nil {
		t.Fatalf("快取命中失敗: %v", err)
	}
	if calls != 1 {
		t.Fatalf("TTL 內第二次查詢不應打來源，呼叫次數=%d", calls)
	}
	if q2.Source != q1.Source || q2.Stale {
		t.Fatalf("快取命中內容錯: %+v", q2)
	}

	// 來源掛掉 + TTL 過期 → 過期快取 fallback
	time.Sleep(2 * time.Millisecond) // 讓快取超過 TTL=1ms
	c2 := &Catalog{CacheDir: dir, TTL: time.Millisecond, AWSQuery: &QueryClient{Endpoint: "http://127.0.0.1:1/"}}
	stale, err := c2.Quote(context.Background(), FamEBS, attrs)
	if err != nil {
		t.Fatalf("離線應回過期快取，得錯誤: %v", err)
	}
	if !stale.Stale || stale.UnitPrice != 0.08 {
		t.Fatalf("過期快取標注錯: %+v", stale)
	}
}

// 快取不存在且來源掛掉 → 回傳原始錯誤。
func TestCatalogOfflineNoCache(t *testing.T) {
	c := &Catalog{CacheDir: filepath.Join(t.TempDir(), "empty"), AWSQuery: &QueryClient{Endpoint: "http://127.0.0.1:1/"}}
	_, _, err := c.Price(context.Background(), FamEC2, Attrs{"instance_type": "m5.large"})
	if err == nil {
		t.Fatal("無快取且來源不可達應回錯誤")
	}
}

func TestCatalogAlicloudNoKeys(t *testing.T) {
	c := &Catalog{} // Ali 未配置
	if _, _, err := c.Price(context.Background(), Family("ecs"), Attrs{"cloud": "alicloud"}); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("應 ErrNoCredentials，得 %v", err)
	}
}

func TestCatalogUnsupportedFamily(t *testing.T) {
	c := &Catalog{}
	_, _, err := c.Price(context.Background(), Family("lambda"), Attrs{})
	if err == nil || !os.IsNotExist(err) && !errors.Is(err, ErrNotFound) {
		// v1 未支援家族：明確報錯即可
		if err == nil {
			t.Fatal("未支援家族應回錯誤")
		}
	}
}

// jsonQuote 把字串包成 JSON 字串字面值（模擬 PriceList 元素的內嵌 JSON）。
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
