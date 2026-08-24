package pricing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// FileCache 為檔案型快取：TTL 內直接命中、不重複下載；
// TTL 過期或離線時，呼叫端可取用過期內容並標注 stale。
type FileCache struct {
	Dir string // 空字串＝停用快取
}

type cacheEnvelope struct {
	StoredAt time.Time       `json:"stored_at"`
	Payload  json.RawMessage `json:"payload"`
}

func (c *FileCache) key(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(c.Dir, hex.EncodeToString(sum[:16])+".json")
}

// Store 寫入 payload 並記錄時間。
func (c *FileCache) Store(key string, payload []byte) error {
	if c.Dir == "" {
		return nil
	}
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return fmt.Errorf("pricing: 建立快取目錄: %w", err)
	}
	raw, err := json.Marshal(cacheEnvelope{StoredAt: time.Now().UTC(), Payload: payload})
	if err != nil {
		return err
	}
	return os.WriteFile(c.key(key), raw, 0o644)
}

// LoadResult 為快取讀取結果。
type LoadResult struct {
	Payload json.RawMessage
	Fresh   bool // true = 尚在 TTL 內
	OK      bool // false = 快取不存在
}

// Load 讀取快取；fresh 判定以 ttl 為準。離線場景呼叫端拿 OK=true、Fresh=false 的結果使用並標注 stale。
func (c *FileCache) Load(key string, ttl time.Duration) LoadResult {
	if c.Dir == "" {
		return LoadResult{}
	}
	raw, err := os.ReadFile(c.key(key))
	if err != nil {
		return LoadResult{}
	}
	var env cacheEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return LoadResult{}
	}
	fresh := time.Since(env.StoredAt) < ttl
	return LoadResult{Payload: env.Payload, Fresh: fresh, OK: true}
}

// CacheKey 為查詢產生穩定快取鍵（屬性排序後串接）。
func CacheKey(family Family, attrs Attrs) string {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s := string(family)
	for _, k := range keys {
		s += "|" + k + "=" + attrs[k]
	}
	return s
}
