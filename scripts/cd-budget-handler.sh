#!/usr/bin/env bash
# cd-budget-handler.sh——成本/預算 CD 閘門腳本（F6 Phase 1，notify 模式）
#
# 契約：查詢 slo-sentinel 唯讀端點，依狀態輸出警告；notify 模式下「永不阻擋」。
#
# 用法：
#   SENTINEL_URL=https://sentinel.internal SLO_ID=api-availability ./cd-budget-handler.sh
#   （或）./cd-budget-handler.sh https://sentinel.internal api-availability
#
# 環境變數：
#   SENTINEL_URL   sentinel 位址（必填；優先於 $1）
#   SLO_ID         感測 id（必填；優先於 $2）
#   GITHUB_SUMMARY 設為 1 時額外寫入 $GITHUB_STEP_SUMMARY（GitHub Actions 用）
#   BUDGET_ENFORCE 設為 1 且狀態 critical → exit 1（T021 enforce 預留；預設 0＝永不阻擋）
#
# 失敗語意（fail-open）：任何錯誤（連不上、解析失敗）→ 警告後 exit 0，
# 不阻擋部署。唯一會回傳非零的情況：BUDGET_ENFORCE=1 且 state=critical。
#
# jq 缺漏備援：無 jq 時退回 grep/sed 解析（僅解析平鋪欄位，夠用於本契約）。

set -u

SENTINEL_URL="${SENTINEL_URL:-${1:-}}"
SLO_ID="${SLO_ID:-${2:-}}"

warn() { echo "::warning::$*" >&2; echo "$*"; }
info() { echo "$*"; }

if [ -z "${SENTINEL_URL}" ] || [ -z "${SLO_ID}" ]; then
    warn "budget-check: 未設定 SENTINEL_URL / SLO_ID——跳過預算檢查（fail-open）"
    exit 0
fi

url="${SENTINEL_URL%/}/api/budget-status/${SLO_ID}"

resp="$(curl -sf --max-time 5 "$url")"
curl_rc=$?
if [ $curl_rc -ne 0 ] || [ -z "$resp" ]; then
    warn "budget-check: sentinel 無法連線或逾時(rc=${curl_rc})——跳過預算檢查（fail-open）"
    exit 0
fi

# ---- 解析（jq 優先，缺漏時 grep/sed 備援）----
if command -v jq >/dev/null 2>&1 && [ "${FORCE_NO_JQ:-0}" != "1" ]; then
    state=$(printf '%s' "$resp" | jq -r '.state // empty')
    remain=$(printf '%s' "$resp" | jq -r '.remaining_budget // empty')
    eta=$(printf '%s' "$resp" | jq -r 'if .eta_hours == null then "n/a" else ((.eta_hours | floor | tostring) + "h") end')
    confirmed=$(printf '%s' "$resp" | jq -r '.confirmed_date // empty')
else
    # 備援：平鋪 JSON 的欄位擷取（值不含引號/逗號即可靠）
    get_str() {
        printf '%s' "$resp" | tr ',' '\n' | awk -v key="\"$1\":" '
            index($0, key) {
                s = substr($0, index($0, key) + length(key))
                gsub(/^[ "]+/, "", s)
                sub(/[",}].*/, "", s)
                print s
                exit
            }'
    }
    state=$(get_str state)
    remain=$(get_str remaining_budget)
    confirmed=$(get_str confirmed_date)
    eta_raw=$(get_str eta_hours)
    if [ -n "$eta_raw" ] && [ "$eta_raw" != "null" ]; then
        eta="$(printf '%s' "$eta_raw" | sed 's/\..*//' )h"
    else
        eta="n/a"
    fi
fi

if [ -z "$state" ]; then
    warn "budget-check: 回應解析失敗——跳過預算檢查(fail-open); raw=${resp:0:120}"
    exit 0
fi

summary_line="slo-sentinel [${state}] ${SLO_ID}: 餘量 ${remain:-?}%｜ETA ${eta:-n/a}｜帳務截至 ${confirmed:-n/a}"

case "$state" in
    healthy)
        info "✅ budget-check: ${summary_line}"
        ;;
    warning)
        msg="預算狀態 warning：${SLO_ID} 餘量僅剩 ${remain:-?}%（ETA ${eta}）。發版前請確認不會加速消耗。"
        warn "⚠️ budget-check: ${msg}"
        [ "${GITHUB_SUMMARY:-0}" = "1" ] && { echo "### ⚠️ $msg" >> "${GITHUB_STEP_SUMMARY:-/dev/null}"; }
        ;;
    critical)
        msg="預算狀態 critical：${SLO_ID} 餘量 ${remain:-?}%、ETA ${eta}。強烈建議暫緩非必要發版(notify 模式: 本次不阻擋)。"
        warn "🔴 budget-check: ${msg}"
        [ "${GITHUB_SUMMARY:-0}" = "1" ] && { echo "### 🔴 $msg" >> "${GITHUB_STEP_SUMMARY:-/dev/null}"; }
        if [ "${BUDGET_ENFORCE:-0}" = "1" ]; then
            echo "🛑 BUDGET_ENFORCE=1 且 critical——阻擋部署（exit 1）"
            exit 1
        fi
        ;;
    *)
        warn "budget-check：未知狀態 '$state'——視為 healthy 處理（fail-open）"
        ;;
esac

exit 0
