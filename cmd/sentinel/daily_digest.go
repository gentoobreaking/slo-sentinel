package main

// daily_digest.go（T025）：每日固定時間發送一封全感測狀態彙總摘要。
//
// 模式照抄 maybeWeeklyCost：store 持久化去重（同日重啟不重發）、
// 發送失敗不登記、下輪重試。時刻可由 config `daily_digest_time` 或
// 環境變數 DAILY_DIGEST 覆寫；DAILY_DIGEST=off 完全停用。
// 內容：各感測現況（alert.Digest.Format）＋與上次摘要相比的狀態變化
// （上次快照持久化於 store，重啟不丟）。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"slo-sentinel/internal/alert"
	"slo-sentinel/internal/store"
)

const (
	digestStateID  = "__daily_digest__"        // 最後發送日（YYYY-MM-DD）
	digestSnapID   = "__daily_digest_states__" // 上次摘要時的狀態快照（JSON）
	digestDisabled = ""
)

// parseDigestTime 解析 HH:MM（24 小時制）。
func parseDigestTime(s string) (int, int, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// maybeDailyDigest 每輪檢查是否已達今日發送時刻；到點且當日未發才送。
func (d *daemon) maybeDailyDigest(ctx context.Context, now time.Time) {
	if d.digestTime == digestDisabled {
		return // off / 未設定
	}
	h, m, ok := parseDigestTime(d.digestTime)
	if !ok {
		return
	}
	local := now.Local()
	sendAt := time.Date(local.Year(), local.Month(), local.Day(), h, m, 0, 0, local.Location())
	if local.Before(sendAt) {
		return // 還沒到今天的發送時刻
	}

	dayKey := now.Format("2006-01-02")
	if prev, _ := d.st.GetState(digestStateID); prev != nil && prev.State == dayKey {
		return // 今日已發（含同日重啟）
	}

	current := map[string]string{}
	d.eachState(func(st store.SensorState) { current[st.SensorID] = st.State })

	var b strings.Builder
	b.WriteString(alert.Digest{}.Format(current))
	if changed := d.digestChanges(current); len(changed) > 0 {
		b.WriteString("\n過去一天狀態變化：\n")
		for _, line := range changed {
			fmt.Fprintf(&b, "  %s\n", line)
		}
	} else {
		b.WriteString("\n過去一天無狀態變化\n")
	}

	// 發送失敗不登記，下一輪自動重試（T026 同款保護）
	if err := d.notifier.Send(ctx, b.String()); err != nil {
		d.log.Error("daily_digest_send_failed", "error", err.Error())
		return
	}
	_ = d.st.SetState(store.SensorState{SensorID: digestStateID, State: dayKey, LastNotifyAt: now.UTC()})
	if snap, err := json.Marshal(current); err == nil {
		_ = d.st.SetState(store.SensorState{SensorID: digestSnapID, State: string(snap), LastNotifyAt: now.UTC()})
	}
	d.log.Info("daily_digest_sent", "day", dayKey, "sensors", len(current))
}

// digestChanges 比對目前狀態與上次成功摘要的快照，回傳變化清單（排序後）。
func (d *daemon) digestChanges(current map[string]string) []string {
	snap, err := d.st.GetState(digestSnapID)
	if err != nil || snap == nil || snap.State == "" {
		return nil // 首次發送無基準
	}
	var old map[string]string
	if json.Unmarshal([]byte(snap.State), &old) != nil {
		return nil
	}
	var out []string
	for id, st := range current {
		if o, ok := old[id]; ok && o != st {
			out = append(out, fmt.Sprintf("%s：%s → %s", id, o, st))
		}
	}
	sort.Strings(out)
	return out
}
