// Package pricing 提供公開價目表的查詢、解析與快取（F11 estimate 模式主路徑）。
//
// 核心約束（algs/cost-forecast.md §D.0）：維護人員通常沒有 billing IAM，
// 成本可見性的主路徑不能依賴帳務 API——以「用量指標 × 公開單價」推估花費。
//
// 資料源：
//   - AWS Price List Query API（免認證，輕量首選）
//   - AWS Price List Bulk API（index.json → 各 region offer 檔，串流解析）
//   - 阿里雲 BSSOpenAPI QuerySkuPriceList（唯讀 RAM key 即可）
//
// v1 固定家族：EC2 按時租、EBS 每 GB-月、RDS 儲存；其餘列 v2。
// 幣別如實保留原幣（§D.4：多幣別只記原幣＋一個設定匯率換算）。
package pricing
