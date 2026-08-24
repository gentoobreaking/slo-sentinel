#!/usr/bin/env python3
"""mock_llm.py（T020 E2E 演練用）：OpenAI 相容的假 LLM 端點。

回傳符合 ai-oncall TriageReport schema 的固定報告（incident_id 從 prompt 抽取），
讓 core 的分診管線（context 收集 → LLM → schema 驗證）可離線全鏈路演練。
僅供本機測試——生產環境請接真實 provider。
"""
import json
import re
from http.server import BaseHTTPRequestHandler, HTTPServer


def build_report(prompt: str) -> dict:
    m = re.search(r"inc-[0-9a-f]+", prompt)
    inc = m.group(0) if m else "inc-unknown"
    return {
        "incident_id": inc,
        "hypotheses": [
            {
                "cause": "容量成長速率超過現有規劃（sentinel ETA 觸頂預警，E2E 演練）",
                "confidence": 0.85,
                "evidence": [
                    "labels: alertname=CapacityEtaWarning severity=critical",
                    f"labels: {next((l for l in ('service=storage-api', 'scope=k8s') if l in prompt), 'service 未攜帶')}",
                    "context: Prometheus availability/request_rate 序列（見 context bundle）",
                ],
            },
            {
                "cause": "HPA 擴容上限或 quota 不足導致無法消化流量",
                "confidence": 0.4,
                "evidence": [
                    "kube_deployment_status_replicas 若缺漏會列於 degraded_sources",
                ],
            },
        ],
        "suggested_actions": [
            {
                "action": "檢查 HPA 目標值與近期擴縮容軌跡；確認 PVC/節點擴容計畫（唯讀調查）",
                "risk": "read-only",
                "runbook_ref": None,
            }
        ],
        "missing_context": [
            "kube_deployment_status_replicas 序列（本機演練無 kube-state-metrics）",
            "quota 使用量序列未設定",
        ],
        "prompt_version": "1.0.0",
    }


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):  # noqa: N802
        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length) or b"{}"
        try:
            body = json.loads(raw)
        except ValueError:
            body = {}
        prompt = "".join(m.get("content", "") for m in body.get("messages", []))
        content = json.dumps(build_report(prompt), ensure_ascii=False)
        resp = {
            "choices": [{"message": {"role": "assistant", "content": content}}],
            "usage": {"total_tokens": len(content) // 4},
        }
        data = json.dumps(resp).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):  # 靜音 access log
        pass


if __name__ == "__main__":
    import sys

    port = int(sys.argv[1]) if len(sys.argv) > 1 else 18000
    HTTPServer(("127.0.0.1", port), Handler).serve_forever()
