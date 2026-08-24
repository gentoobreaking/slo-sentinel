package pricing

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// fixture：縮小版 index.json——真實檔的 offers.<service>.regions 結構。
func indexFixture() []byte {
	return []byte(`{
	  "formatVersion": "v1.0",
	  "offers": {
	    "AmazonEC2": {
	      "offerCode": "AmazonEC2",
	      "currentRegionIndexUrl": "https://pricing.example/AmazonEC2/current/region_index.json",
	      "currentVersionIndexUrl": "https://pricing.example/AmazonEC2/current/index.json",
	      "regions": {
	        "us-east-1": "https://pricing.example/AmazonEC2/current/us-east-1/index.json",
	        "ap-east-1": "https://pricing.example/AmazonEC2/current/ap-east-1/index.json"
	      }
	    },
	    "AmazonS3": {
	      "offerCode": "AmazonS3",
	      "currentRegionIndexUrl": "https://pricing.example/AmazonS3/current/region_index.json"
	    }
	  }
	}`)
}

func TestLocateOfferURL(t *testing.T) {
	idx := indexFixture()

	u, err := LocateOfferURL(idx, "AmazonEC2", "ap-east-1")
	if err != nil || u != "https://pricing.example/AmazonEC2/current/ap-east-1/index.json" {
		t.Fatalf("region 命中: u=%q err=%v", u, err)
	}

	// region 未列出 → 回退 currentRegionIndexUrl
	u, err = LocateOfferURL(idx, "AmazonS3", "eu-central-1")
	if err != nil || u != "https://pricing.example/AmazonS3/current/region_index.json" {
		t.Fatalf("region 回退: u=%q err=%v", u, err)
	}

	// 未知服務
	if _, err := LocateOfferURL(idx, "Nope", "us-east-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知服務應 ErrNotFound，得 %v", err)
	}

	// 壞 JSON
	if _, err := LocateOfferURL([]byte("{bad"), "AmazonEC2", "us-east-1"); err == nil {
		t.Fatal("壞 JSON 應回傳錯誤")
	}
}

// offerFixture 產生含 n 個產品（僅一個符合 m5.large）＋對應 terms 的合成 offer 內容。
func offerFixture(n int, matchEvery int) (string, int) {
	var b strings.Builder
	b.WriteString(`{"formatVersion":"v1.0","disclaimer":"test","products":{`)
	matches := 0
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		sku := skuFor(i)
		isMatch := matchEvery > 0 && i%matchEvery == 0
		if isMatch {
			matches++
			b.WriteString(`"` + sku + `":{"sku":"` + sku + `","productFamily":"Compute Instance","attributes":{"instanceType":"m5.large","operatingSystem":"Linux","capacitystatus":"Used","preInstalledSw":"NA","regionCode":"us-east-1"}}`)
		} else {
			b.WriteString(`"` + sku + `":{"sku":"` + sku + `","productFamily":"Compute Instance","attributes":{"instanceType":"x` + itoa(i) + `.huge","operatingSystem":"Windows","regionCode":"us-west-2"}}`)
		}
	}
	b.WriteString(`},"terms":{"OnDemand":{`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		sku := skuFor(i)
		price := "0.096"
		if matchEvery > 0 && i%matchEvery == 0 {
			price = "0.123"
		}
		b.WriteString(`"` + sku + `":{"` + sku + `.TERM.JRTCKXETXF":{"offerTermCode":"JRTCKXETXF","effectiveDate":"2024-01-01T00:00:00Z","priceDimensions":{"` + sku + `.TERM.JRTCKXETXF.DIM1":{"unit":"Hrs","description":"per hour","pricePerUnit":{"USD":"` + price + `"}}}}}`)
	}
	b.WriteString(`}},"publicationDate":"2024-06-01T00:00:00Z"}`)
	return b.String(), matches
}

func skuFor(i int) string { return "SKU" + strings.Repeat("X", 3) + itoa(i) }

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func matcher() ProductMatcher {
	return func(sku, family string, a map[string]string) bool {
		return family == "Compute Instance" &&
			a["instanceType"] == "m5.large" && a["operatingSystem"] == "Linux"
	}
}

func TestFindOnDemandRatesStreams(t *testing.T) {
	raw, _ := offerFixture(500, 10) // 50 個符合

	rates, err := FindOnDemandRates(strings.NewReader(raw), matcher(), FindOptions{MaxResults: 5})
	if err != nil {
		t.Fatalf("解析失敗: %v", err)
	}
	if len(rates) != 5 { // MaxResults 上限生效
		t.Fatalf("MaxResults=5 應只保留 5 筆，得 %d", len(rates))
	}
	r := rates[0]
	if r.Unit != "Hrs" || r.Currency != "USD" || r.PricePerUnit != 0.123 {
		t.Fatalf("費率內容錯: %+v", r)
	}
}

func TestFindOnDemandRatesNotFound(t *testing.T) {
	raw, _ := offerFixture(100, 0) // 全部不符合
	if _, err := FindOnDemandRates(strings.NewReader(raw), matcher(), FindOptions{MaxResults: 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("應 ErrNotFound，得 %v", err)
	}
}

// chunkReader 以固定大小分塊供應資料，並記錄最大單次 Read 請求量——
// 驗證解析器以串流方式讀取（不會一次 slurp 整檔）。
type chunkReader struct {
	src      string
	pos      int
	maxRead  int
	maxChunk int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(p) > c.maxRead {
		c.maxRead = len(p)
	}
	n := copy(p, c.src[c.pos:])
	c.pos += n
	if c.pos >= len(c.src) {
		return n, io.EOF
	}
	return n, nil
}

func TestFindOnDemandMemoryBound(t *testing.T) {
	raw, _ := offerFixture(2000, 20) // 大輸入（數 MB），只有 100 個符合
	cr := &chunkReader{src: raw}

	rates, err := FindOnDemandRates(cr, matcher(), FindOptions{MaxResults: 3})
	if err != nil {
		t.Fatalf("解析失敗: %v", err)
	}
	if len(rates) != 3 {
		t.Fatalf("應受 MaxResults=3 上限約束，得 %d", len(rates))
	}
	// bufio 固定 64KB → 最大單次讀取不得超過它
	if cr.maxRead > 64<<10 {
		t.Fatalf("單次 Read %d bytes 超過 64KB 緩衝——記憶體峰值失控", cr.maxRead)
	}
}
