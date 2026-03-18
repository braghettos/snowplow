#!/usr/bin/env bash
# Run snowplow_test.py (unified test suite) with Playwright support.
# Creates .venv-bench if missing and installs playwright + chromium.
set -e
cd "$(dirname "$0")/../.."
if [[ ! -d .venv-bench ]]; then
  echo "Creating .venv-bench and installing playwright..."
  python3 -m venv .venv-bench
  .venv-bench/bin/pip install -q -r e2e/bench/requirements.txt
  .venv-bench/bin/playwright install chromium
fi
exec .venv-bench/bin/python e2e/bench/snowplow_test.py "$@"
