#!/usr/bin/env bash
# 上游同步腳本：拉取 awesome-prometheus-alerts 最新版至 rules.d/community/
# 並將 commit hash 記錄於 UPSTREAM_COMMIT（algs/sensor-catalog.md §C.5）
set -euo pipefail
REPO="https://github.com/samber/awesome-prometheus-alerts.git"
DEST="$(dirname "$0")/../rules.d/community"
UPSTREAM="$(dirname "$0")/../rules.d/community/UPSTREAM_COMMIT"

if [ -d "$DEST/.git" ]; then
  cd "$DEST" && git pull --ff-only
else
  git clone --depth 1 "$REPO" "$DEST"
fi
COMMIT=$(cd "$DEST" && git rev-parse HEAD)
echo "$COMMIT" > "$UPSTREAM"
echo "✅ community/ 同步完成，上游 commit：$COMMIT"
echo "⚠️  請審查 git -C $DEST log 的差異後再提交變更"
