// Package promdur 解析 Prometheus 風格時長：支援 y/w/d/h/m/s 組合（如 72h、14d、2w）。
// Go 原生 time.ParseDuration 不支援 d/w/y。無法解析或含負值時回傳 0。
package promdur

import (
	"strconv"
	"strings"
	"time"
)

func Parse(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var total time.Duration
	var num strings.Builder
	flush := func(unit string) bool {
		if num.Len() == 0 {
			return false
		}
		n, err := strconv.ParseFloat(num.String(), 64)
		num.Reset()
		if err != nil || n < 0 {
			return false
		}
		switch unit {
		case "y":
			total += time.Duration(n * 365 * 24 * float64(time.Hour))
		case "w":
			total += time.Duration(n * 7 * 24 * float64(time.Hour))
		case "d":
			total += time.Duration(n * 24 * float64(time.Hour))
		case "h":
			total += time.Duration(n * float64(time.Hour))
		case "m":
			total += time.Duration(n * float64(time.Minute))
		case "s":
			total += time.Duration(n * float64(time.Second))
		default:
			return false
		}
		return true
	}
	for _, ch := range s {
		if (ch >= '0' && ch <= '9') || ch == '.' {
			num.WriteRune(ch)
			continue
		}
		if !flush(string(ch)) {
			return 0
		}
	}
	if num.Len() > 0 { // 純數字無單位：視為秒（同 Prometheus 約定）
		n, err := strconv.ParseFloat(num.String(), 64)
		if err != nil {
			return 0
		}
		total += time.Duration(n * float64(time.Second))
	}
	return total
}
