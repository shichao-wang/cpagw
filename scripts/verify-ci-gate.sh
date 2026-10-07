#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -eq 0 ]]; then
  echo "用法：verify-ci-gate.sh <job-result>..." >&2
  exit 2
fi

for result in "$@"; do
  if [[ "$result" != "success" ]]; then
    echo "合并门禁未通过：依赖 job 结果为 ${result:-<empty>}，必须全部为 success。" >&2
    exit 1
  fi
done

echo "所有合并前检查均为 success。"
