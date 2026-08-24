package pricing

import (
	"errors"
	"fmt"
)

// Family 為 v1 支援的價目家族（任務書備註：EC2 按時租、EBS 每 GB、RDS 儲存）。
type Family string

const (
	FamEC2 Family = "ec2" // 按時租 USD/hr；attrs: region, instance_type, os（預設 Linux）
	FamEBS Family = "ebs" // 每 GB-月 USD；attrs: region, volume_type（預設 gp3）
	FamRDS Family = "rds" // 儲存每 GB-月 USD；attrs: region, engine, volume_type
)

// ErrNotFound 表示價目表中找不到符合條件的 SKU。
var ErrNotFound = errors.New("pricing: 查無符合條件的單價")

// ErrNoCredentials 表示該來源需要金鑰但未設定（阿里雲唯讀 RAM key）。
var ErrNoCredentials = errors.New("pricing: 未設定來源所需金鑰")

// Attrs 為查詢屬性（region / instance_type / volume_type / cloud …）。
type Attrs map[string]string

func (a Attrs) get(key, def string) string {
	if v := a[key]; v != "" {
		return v
	}
	return def
}

// Quote 為一次單價查詢的結果。
type Quote struct {
	UnitPrice float64 // 原幣單價
	Currency  string  // 原幣別（USD / CNY…）——§D.4 不在此換算
	Unit      string  // 計價單位（Hrs / GB-Mo / 個…），供 estimate 公式對齊用量單位
	Source    string  // 來源標注：aws-query / aws-bulk / alicloud-sku / cache:<src>
	Stale     bool    // 離線時使用過期快取＝true
}

// String 人話輸出，供報表與 UI 對照。
func (q Quote) String() string {
	stale := ""
	if q.Stale {
		stale = "（過期快取）"
	}
	return fmt.Sprintf("%s %.4f/%s [%s]%s", q.Currency, q.UnitPrice, q.Unit, q.Source, stale)
}
