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
  if [[ "${MOCK_PID:-}" != "" ]]; then kill "$MOCK_PID" 2>/dev/null || true; fi
  if [[ "${DEF_BAK:-}" != "" && -f "$DEF_BAK" ]]; then
    mv "$DEF_BAK" "$DEF"
    echo "↩︎ 已還原 $DEF"
  fi
}
trap cleanup EXIT

echo "== 1/4 啟動 prometheus + node-exporter（slo-sentinel dev profile）=="
docker compose -f "$SENTINEL_ROOT/docker-compose.yml" --profile dev up -d prometheus node-exporter

echo "== 2/4 啟動 ai-oncall gate + core（mock LLM → 真分診管線）=="
python3 "$SENTINEL_ROOT/scripts/mock_llm.py" 18000 &
MOCK_PID=$!
sleep 1
(
  cd "$ONCALL_ROOT/deploy"
  export SHARED_SECRET="$SECRET" PROMETHEUS_URL=http://host.docker.internal:9090
  docker compose down -v >/dev/null 2>&1 || true   # 清 incident/gate 狀態：每次演練乾淨起點
  LLM_PROVIDERS="mock|http://host.docker.internal:18000/v1|mock-model|sk-mock" \
    SHADOW_MODE=0 EXECUTOR_MODE=log-only GATE_ADDR=gate:50060 \
    docker compose up -d --build --force-recreate gate core
)

if [[ "$FORCE_CRITICAL" == "1" ]]; then
  echo "== 2.5 換上合成感測（time() 線性成長，約 3 分鐘內必觸發 critical）=="
  DEF_BAK="$(mktemp /private/tmp/e2e-def-bak.XXXXXX)"
  cp "$DEF" "$DEF_BAK"
  NOW_PLUS=$(( $(date +%s) + 240 ))
  cat > "$DEF" <<EOF
# E2E 演練用合成感測（T020 腳本自動產生，結束自動還原）
sensors:
  - id: dev-root-disk
    service: storage-api
    scope: k8s
    metric:
      value:   'vector(time())'          # 每秒 +1 的確定成長序列（vector 包裝避免 scalar 回應）
      ceiling: 'vector($NOW_PLUS)' # 固定天花板：4 分鐘後觸頂
    horizons: [6h]
    thresholds:
      warn_eta: 90s
      crit_eta: 45s
EOF
  echo "thresholds 已收緊：warn_eta=90s crit_eta=45s，天花板 = now+240s"
fi

echo "== 3/4 啟動 sentinel（ONCALL_GATE_URL 指向 gate；強制重建以載入新定義）=="
cd "$SENTINEL_ROOT"
ONCALL_GATE_URL=http://host.docker.internal:8080/alerts ONCALL_GATE_TOKEN="$SECRET" \
  docker compose up -d --build --force-recreate sentinel

echo "== 4/4 等待輪詢與分診（約 5 分鐘），然後查驗 =="
sleep 300
echo "---- sentinel 感測狀態 ----"
curl -s http://127.0.0.1:9099/api/status.json; echo
echo "---- sentinel 最近日誌（triage 相關）----"
docker logs slo-sentinel 2>&1 | grep -iE "triage|notify_failed" | tail -5 || true
echo "---- ai-oncall incidents（core readapi）----"
docker exec oncall-core python3 -c \
  "import urllib.request;print(urllib.request.urlopen('http://127.0.0.1:8090/api/incidents').read().decode())"
echo "---- 分診報告（timeline / shadow report）----"
INC=$(docker exec oncall-core python3 -c "import json,urllib.request;d=json.load(urllib.request.urlopen('http://127.0.0.1:8090/api/incidents'));print(d['items'][0]['id'] if d['items'] else '')")
if [[ -n "$INC" ]]; then
  docker exec oncall-core python3 -c "import urllib.request;print(urllib.request.urlopen('http://127.0.0.1:8090/api/incidents/$INC').read().decode())" | head -60
  docker exec oncall-core sh -c 'ls /data/shadow_reports 2>/dev/null | tail -2' || true
fi
echo "---- gate 推播日誌（DeliverNotification）----"
docker logs oncall-gate 2>&1 | grep -iE "deliver|triage|report" | tail -4 || true

cat <<'EOF'

演練判讀：
- /api/status.json 中 data-disk 應為 warning/critical
- sentinel 日誌應出現 triage 轉交成功（無 triage_publish_failed）
- /api/incidents 應出現 CapacityEtaWarning 的 incident 與分診報告
  （FakeProvider 影子報告；HPA/quota context 視 Prometheus 實際序列，
   缺漏會列在 degraded_sources——屬預期行為）
- Telegram 若已設定，本地只會收到精簡卡「已轉交 ai-oncall 分診」
EOF
