#!/bin/sh
# 容器進入點：修正資料卷權限後以非 root（sentinel）執行。
# 若以自訂 user 執行（非 root），直接透傳不做權限操作。

set -e

DATA_DIR=/var/lib/sentinel

if [ "$(id -u)" = "0" ]; then
    mkdir -p "$DATA_DIR/pricing-cache"
    chown -R sentinel:sentinel "$DATA_DIR"
    exec su-exec sentinel "$@"
fi

exec "$@"
