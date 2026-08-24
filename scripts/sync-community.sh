#!/usr/bin/env bash
# 上游同步腳本：從 awesome-prometheus-alerts「挑選」需要的規則到 rules.d/community/
#
# 設計（algs/sensor-catalog.md §C.5）：
#   - 上游完整 repo 快取在 .community-upstream/（於 sentinel 載入路徑之外，
#     否則 filepath.WalkDir 會把 93 個服務的規則全吃進 catalog）
#   - 只有 rules.d/community/SELECTED 清單中列出的服務會被複製進載入路徑
#
# 用法：
#   1. echo "redis"        >> rules.d/community/SELECTED   # 一行一個服務名
#      echo "postgresql"   >> rules.d/community/SELECTED    # 名稱見上游 dist/rules/ 目錄
#   2. ./scripts/sync-community.sh
#
# 同步後請審查 git diff 再提交；UPSTREAM_COMMIT 記錄來源版本。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CACHE="$ROOT/.community-upstream"
DEST="$ROOT/rules.d/community"
SELECTED="$DEST/SELECTED"
UPSTREAM="$DEST/UPSTREAM_COMMIT"
REPO="https://github.com/samber/awesome-prometheus-alerts.git"

if [ ! -f "$SELECTED" ]; then
    echo "❌ 找不到 $SELECTED" >&2
    echo "   請先建立挑選清單（一行一個服務名，名稱見上游 dist/rules/ 目錄）：">&2
    echo "   例：printf 'redis\npostgresql\n' > $SELECTED" >&2
    exit 1
fi

# ---- 上游快取（載入路徑之外）----
if [ -d "$CACHE/.git" ]; then
    git -C "$CACHE" pull --ff-only >/dev/null
else
    rm -rf "$CACHE"
    git clone --depth 1 "$REPO" "$CACHE" >/dev/null
fi
COMMIT=$(git -C "$CACHE" rev-parse HEAD)

# ---- 依清單挑選複製 ----
mkdir -p "$DEST"
copied=0
while IFS= read -r svc; do
    svc="$(echo "$svc" | tr -d '[:space:]')"
    [ -z "$svc" ] && continue
    case "$svc" in \#*) continue ;; esac   # 允許 # 註解

    src_dir="$CACHE/dist/rules/$svc"
    if [ ! -d "$src_dir" ]; then
        echo "⚠️  上游無此服務目錄：${svc}（跳過；名稱見 $CACHE/dist/rules/）" >&2
        continue
    fi
    # 上游副檔名混用 .yml/.yaml——兩者都收（載入器兩者皆支援）
    found=$(find "$src_dir" -name "*.y*ml" | wc -l | tr -d ' ')
    if [ "$found" -eq 0 ]; then
        echo "⚠️  $svc 目錄內無 rules 檔（跳過）" >&2
        continue
    fi
    find "$src_dir" -name "*.y*ml" -exec cp -f {} "$DEST/" \;
    copied=$((copied + found))
    echo "✅ ${svc}：$found 個檔案"
done < "$SELECTED"

# ---- 自動分類：為挑入的規則補 sentinel_kind（使用者免判斷）----
if command -v go >/dev/null 2>&1; then
    # 預設建置快取不可寫時（macOS TCC）退回專案內目錄
    export GOCACHE="${GOCACHE:-$ROOT/.gocache}"
    mkdir -p "$GOCACHE"
    (cd "$ROOT" && go run ./cmd/ruleclassify -dir "$DEST")
else
    echo "⚠️  未安裝 Go——跳過自動分類（規則以 KindNone 載入，不影響其他感測）" >&2
fi

echo "$COMMIT" > "$UPSTREAM"
echo "✅ 同步完成：共 $copied 個檔案｜上游 commit：${COMMIT:0:12}"
echo "⚠️  請審查 git diff 後再提交變更"
