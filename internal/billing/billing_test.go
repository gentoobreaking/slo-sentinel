package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAWSCostExplorerParsesAndSigns(t *testing.T) {
	var gotAuth, gotBody, gotTarget string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		gotBody = string(body)
		gotTarget = r.Header.Get("X-Amz-Target")
		w.Write([]byte(`{"ResultsByTime":[
		  {"TimePeriod":{"Start":"2026-08-20"},
		   "Groups":[
		     {"Keys":["Amazon EC2"],"Metrics":{"UnblendedCost":{"Amount":"12.5"}}},
		     {"Keys":["Amazon S3"],"Metrics":{"UnblendedCost":{"Amount":"0.75"}}}
		   ]}
		]}`))
	}))
	defer srv.Close()

	a := &AWSCostExplorer{
		AccessKey: "AKID", SecretKey: "secret", Region: "us-east-1",
		Endpoint: srv.URL, Client: srv.Client(),
	}
	start := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	spends, err := a.DailySpend(context.Background(), Filter{}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(spends) != 2 {
		t.Fatalf("spends = %d", len(spends))
	}
	if spends[0].Service != "Amazon EC2" || spends[0].CostUSD != 12.5 {
		t.Fatalf("unexpected spend: %+v", spends[0])
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AKID/") ||
		!strings.Contains(gotAuth, "ce") {
		t.Fatalf("missing SigV4 authorization: %s", gotAuth)
	}
	if gotTarget != "AWSInsightsFrontendService.GetCostAndUse" {
		t.Fatalf("X-Amz-Target = %s", gotTarget)
	}
	if !strings.Contains(gotBody, `"Granularity":"DAILY"`) {
		t.Fatalf("granularity missing: %s", gotBody)
	}
}

func TestAlicloudBSSSignsAndParses(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`{"Data":{"Items":{"Item":[
		  {"UsageStartDate":"2026-08-20","Cost":3.25}
		]}}}`))
	}))
	defer srv.Close()

	a := &AlicloudBSS{
		AccessKeyID: "ak", AccessKeySecret: "sk",
		Endpoint: srv.URL, Client: srv.Client(),
	}
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	spends, err := a.DailySpend(context.Background(), Filter{}, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(spends) != 1 || spends[0].CostUSD != 3.25 {
		t.Fatalf("spends = %+v", spends)
	}
	if !strings.Contains(gotQuery, "Signature=") ||
		!strings.Contains(gotQuery, "DescribeInstanceBill") {
		t.Fatalf("query missing signature/action: %s", gotQuery)
	}
	if strings.Contains(gotQuery, "%20%2F%26%2F") == false && strings.Count(gotQuery, "Signature=") != 1 {
		t.Fatal("signature format unexpected")
	}
}
