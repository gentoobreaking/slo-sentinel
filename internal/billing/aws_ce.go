package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AWSCostExplorer 以 Cost Explorer API（ce.amazonaws.com，JSON 1.1）實作 BillingSource。
// 資料延遲 ~24h（§D.1）：呼叫端須以回傳的 Date 為準，不得視為即時。
type AWSCostExplorer struct {
	AccessKey, SecretKey string
	Region               string // 如 us-east-1
	Client               *http.Client
	Endpoint             string // 預設 https://ce.amazonaws.com；測試可覆寫
}

func (a *AWSCostExplorer) Name() string { return "aws-ce" }

// DailySpend 呼叫 GetCostAndUse（DAILY/GROUP_BY=SERVICE），轉換為 DailySpend 清單。
func (a *AWSCostExplorer) DailySpend(ctx context.Context, f Filter, start, end time.Time) ([]DailySpend, error) {
	endExclusive := end.AddDate(0, 0, 1) // CE 的 TimePeriod 為含頭不含尾
	reqBody := map[string]any{
		"TimePeriod": map[string]string{
			"Start": start.Format("2006-01-02"),
			"End":   endExclusive.Format("2006-01-02"),
		},
		"Granularity": "DAILY",
		"Metrics":     []string{"UnblendedCost"},
		"GroupBy": []map[string]string{
			{"Type": "DIMENSION", "Key": "SERVICE"},
		},
	}
	for k, v := range f.Tags {
		reqBody["Filter"] = map[string]any{
			"Tags": map[string]any{"Key": k, "Values": []string{v}},
		}
		break // v1：單一 tag filter
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	endpoint := a.Endpoint
	if endpoint == "" {
		endpoint = "https://ce.amazonaws.com"
	}
	host := strings_TrimPrefix(endpoint, "https://")
	signer := sigv4{AccessKey: a.AccessKey, SecretKey: a.SecretKey,
		Region: a.RegionOrDefault(), Service: "ce"}
	headers := signer.sign("POST", host, "/", "", string(body), time.Now().UTC())

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint+"/", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "AWSInsightsFrontendService.GetCostAndUse")

	client := a.Client
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ce request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ce 回應 %d: %s", resp.StatusCode, truncateBytes(raw, 300))
	}

	var ceResp struct {
		ResultsByTime []struct {
			TimePeriod struct {
				Start string `json:"Start"`
			} `json:"TimePeriod"`
			Groups []struct {
				Keys     []string `json:"Keys"`
				Metrics  map[string]struct {
					Amount string `json:"Amount"`
				} `json:"Metrics"`
			} `json:"Groups"`
		} `json:"ResultsByTime"`
	}
	if err := json.Unmarshal(raw, &ceResp); err != nil {
		return nil, fmt.Errorf("decode ce: %w", err)
	}

	var out []DailySpend
	for _, rt := range ceResp.ResultsByTime {
		date, err := time.Parse("2006-01-02", rt.TimePeriod.Start)
		if err != nil {
			return nil, err
		}
		for _, g := range rt.Groups {
			amount := g.Metrics["UnblendedCost"].Amount
			var cost float64
			fmt.Sscanf(amount, "%f", &cost)
			service := ""
			if len(g.Keys) > 0 {
				service = g.Keys[0]
			}
			out = append(out, DailySpend{Date: date, CostUSD: cost, Service: service})
		}
	}
	return out, nil
}

func (a *AWSCostExplorer) RegionOrDefault() string {
	if a.Region == "" {
		return "us-east-1"
	}
	return a.Region
}

func strings_TrimPrefix(s, prefix string) string {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
