package billing

// sigv4：最小化 AWS Signature Version 4 簽名器（僅覆蓋 CE 所需的 POST + JSON body）。
// 不引入 aws-sdk-go-v2（依賴樹過大）；演算法照 AWS 官方規格實作。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

type sigv4 struct {
	AccessKey string
	SecretKey string
	Region    string
	Service   string // "ce"
}

// sign 計算 SigV4 各項 header 值。
func (s sigv4) sign(method, host, path, query, body string, now time.Time) map[string]string {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")

	payloadHash := sha256Hex([]byte(body))

	canonicalHeaders := fmt.Sprintf("content-type:application/x-amz-json-1.1\nhost:%s\nx-amz-date:%s\n",
		host, amzDate)
	signedHeaders := "content-type;host;x-amz-date"

	canonicalRequest := strings.Join([]string{
		method, path, query,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, s.Region, s.Service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+s.SecretKey), dateStamp)
	kRegion := hmacSHA256(kDate, s.Region)
	kService := hmacSHA256(kRegion, s.Service)
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	return map[string]string{
		"Authorization": fmt.Sprintf(
			"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
			s.AccessKey, scope, signedHeaders, signature),
		"X-Amz-Date": amzDate,
	}
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// sortedKeys 供測試與除錯輸出穩定排序。
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
