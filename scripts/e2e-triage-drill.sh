#!/usr/bin/env bash
# e2e-triage-drill.sh（T020 驗收 3）：本機 docker-compose 全鏈路分診演練。
#
# 鏈路：
#   node-exporter → prometheus → sentinel(dev-root-disk 容量感測)
#     --AM webhook 格式轉交--> oncall-gate → oncall-core(FakeProvider 影子分診)
#     → 分診報告（readapi /api/incidents）
#
# 用法：
#   scripts/e2e-triage-drill.sh            # 全自動：強制 critical 觸發轉交
#   FORCE_CRITICAL=0 scripts/...           # 不動門檻，用真實磁碟成長速率慢慢等
#
# 前置：Docker Desktop 已啟動；ai-oncall 在 ~/Projects/ai-oncall（可用 ONCALL_ROOT 覆寫）
set -euo pipefail

SENTINEL_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ONCALL_ROOT="${ONCALL_ROOT:-$HOME/Projects/ai-oncall}"
SECRET="${SHARED_SECRET:-dev-e2e-secret}"
FORCE_CRITICAL="${FORCE_CRITICAL:-1}"
DEF="$SENTINEL_ROOT/capacity_defs/node-disk.yaml"

cleanup() {
  if [[ "${DEF_BAK:-}" != "" && -f "$DEF_BAK" ]]; then
    mv "$DEF_BAK" "$DEF"
    echo "↩︎ 已還原 $DEF"
  fi
}
trap cleanup EXIT

echo "== 1/4 啟動 prometheus + node-exporter（slo-sentinel dev profile）=="
docker compose -f "$SENTINEL_ROOT/docker-compose.yml" --profile dev up -d prometheus node-exporter

echo "== 2/4 啟動 ai-oncall gate + core（未設 LLM_PROVIDERS → FakeProvider 離線模式）=="
docker compose -f "$ONCALL_ROOT/deploy/docker-compose.yml" up -d --build gate core \
  SHARED_SECRET="$SECRET" PROMETHEUS_URL=http://host.docker.internal:9090

if [[ "$FORCE_CRITICAL" == "1" ]]; then
  echo "== 2.5 收緊門檻（T028 熱載入即時生效）讓 ETA 觸頂 critical =="
  DEF_BAK="$(mktemp)"
  cp "$DEF" "$DEF_BAK"
  python3 - "$DEF" <<'PY'
import re, sys
p = sys.argv[1]
s = open(p).read()
s = s.replace("warn_eta: 72h", "warn_eta: 2m").replace("crit_eta: 24h", "crit_eta: 1m")
open(p, "w").write(s)
print("thresholds 已收緊：warn_eta=2m crit_eta=1m")
PY
fi

echo "== 3/4 啟動 sentinel（ONCALL_GATE_URL 指向 gate）=="
cd "$SENTINEL_ROOT"
ONCALL_GATE_URL=http://host.docker.internal:8080 ONCALL_GATE_TOKEN="$SECRET" \
  docker compose up -d --build sentinel

echo "== 4/4 等待輪詢與分診（約 2 分鐘），然後查驗 =="
sleep 120
echo "---- sentinel 感測狀態 ----"
curl -s http://127.0.0.1:9099/api/status.json; echo
echo "---- sentinel 最近日誌（triage 相關）----"
docker logs slo-sentinel 2>&1 | grep -iE "triage|notify_failed" | tail -5 || true
echo "---- ai-oncall incidents（core readapi）----"
docker exec oncall-core python3 -c \
  "import urllib.request;print(urllib.request.urlopen('http://127.0.0.1:8090/api/incidents').read().decode())"

cat <<'EOF'

演練判讀：
- /api/status.json 中 data-disk 應為 warning/critical
- sentinel 日誌應出現 triage 轉交成功（無 triage_publish_failed）
- /api/incidents 應出現 CapacityEtaWarning 的 incident 與分診報告
  （FakeProvider 影子報告；HPA/quota context 視 Prometheus 實際序列，
   缺漏會列在 degraded_sources——屬預期行為）
- Telegram 若已設定，本地只會收到精簡卡「已轉交 ai-oncall 分診」
EOF
