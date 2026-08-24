# 成本/預算 CD 閘門——CI 接入範例（F6 Phase 1，notify 模式）

契約：`GET $SENTINEL_URL/api/budget-status/{slo_id}`
→ `{"mode","state","remaining_budget","eta_hours","confirmed_date",...}`

行為承諾：**notify 模式永不阻擋部署**（exit code 恆 0；連不上 sentinel 也是 fail-open）。
唯一例外：設 `BUDGET_ENFORCE=1` 且 state=critical → exit 1（T021 enforce 模式預留，
需先完成門檻校準與政策審查才允許開啟）。

腳本位置：`scripts/cd-budget-handler.sh`（各 CI 只是把同一支腳本包進不同外殼）。

---

## GitHub Actions

```yaml
# 目標服務 repo 的 deploy workflow 中、deploy step 之前：
  budget-check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4          # 取得 scripts/cd-budget-handler.sh
      - name: slo-sentinel budget status (notify-only)
        env:
          SENTINEL_URL: ${{ secrets.SENTINEL_URL }}   # 內網反代位址
          SLO_ID: api-availability                    # 對應 slo_defs 的 id
          GITHUB_SUMMARY: "1"
        run: bash scripts/cd-budget-handler.sh
```

## GitLab CI

```yaml
budget-check:
  stage: pre-deploy
  image: curlimages/curl:latest   # 或任一含 curl 的映像
  script:
    - export SENTINEL_URL="$SENTINEL_URL" SLO_ID="api-availability"
    - bash scripts/cd-budget-handler.sh
  allow_failure: true             # 雙保險：notify 模式永不影響流水線結果
```

## Jenkins（Declarative Pipeline）

```groovy
stage('budget-check') {
  steps {
    sh '''#!/usr/bin/env bash
      export SENTINEL_URL="$SENTINEL_URL" SLO_ID="api-availability"
      bash scripts/cd-budget-handler.sh
    '''
  }
}
```

## Generic（無 CI 系統／GitOps webhook）

任何會在部署前執行 shell 的地方：

```bash
SENTINEL_URL=https://sentinel.internal SLO_ID=api-availability \
  bash scripts/cd-budget-handler.sh || true   # 保險起見永遠吞掉 exit code
```

Argo CD 使用者可掛 PreSync hook 執行同一支腳本。

---

## 前置檢查清單

1. **網路可達**：runner → sentinel。建議內網或反向代理＋認證（端點唯讀但仍不該公開）
2. **SLO id 一致**：`SLO_ID` 必須等於目標服務 `slo_defs/*.yaml` 的 `id`
3. **fail-open 驗證**：把 SENTINEL_URL 故意填錯跑一次，確認 pipeline 仍綠燈
4. **證據記錄**：成功的 run link 記回 T019 任務書前置條件區
