package main

// llm.go：最小 OpenAI 相容 chat/completions client（不引 SDK）＋輸出抽取。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type genConfig struct {
	URL   string // base_url，如 http://127.0.0.1:11434/v1
	Key   string
	Model string
}

func loadEnv() genConfig {
	return genConfig{
		URL:   strings.TrimRight(os.Getenv("GEN_LLM_URL"), "/"),
		Key:   os.Getenv("GEN_LLM_KEY"),
		Model: os.Getenv("GEN_LLM_MODEL"),
	}
}

func (c genConfig) requireLLM() error {
	if c.URL == "" || c.Model == "" {
		return fmt.Errorf("需要設定 GEN_LLM_URL 與 GEN_LLM_MODEL（OpenAI 相容端點）")
	}
	return nil
}

type llmClient struct {
	cfg  genConfig
	http *http.Client
}

func newLLM(cfg genConfig) *llmClient {
	return &llmClient{cfg: cfg, http: &http.Client{Timeout: 120 * time.Second}}
}

// complete 送出一次 chat completion，回傳 assistant 文字。
func (c *llmClient) complete(ctx context.Context, system, user string) (string, error) {
	payload := map[string]any{
		"model": c.cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.URL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("LLM 回 %d：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("LLM 回應非合法 JSON: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("LLM 回應無 choices")
	}
	return out.Choices[0].Message.Content, nil
}

// extractYAML 從 LLM 輸出抽取 YAML：優先 ```yaml 圍欄，否則整段去雜訊。
func extractYAML(text string) string {
	lines := strings.Split(text, "\n")
	var inFence, inYAML bool
	var buf []string
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if !inFence && (t == "```yaml" || t == "```yml") {
			inFence, inYAML = true, true
			continue
		}
		if inFence && t == "```" {
			inFence = false
			break // 第一個圍欄結束即收工
		}
		if inFence {
			buf = append(buf, line)
		}
	}
	if inYAML && len(buf) > 0 {
		return strings.Join(buf, "\n")
	}
	// 無圍欄：濾掉明顯的非內容行後整段返回
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t != "" && !strings.HasPrefix(t, "（") && !strings.HasPrefix(t, "以下是") {
			buf = append(buf, line)
		}
	}
	return strings.Join(buf, "\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
