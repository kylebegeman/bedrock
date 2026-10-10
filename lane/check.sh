#!/usr/bin/env bash
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
for script in "$here"/*.sh "$here"/vm/boot.sh; do bash -n "$script"; done
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck -S error "$here"/*.sh "$here"/vm/boot.sh
fi
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s "$here" -p 'test_*.py'
python3 - "$here" <<'PY'
import ast
from pathlib import Path
import sys
for path in Path(sys.argv[1]).rglob('*.py'):
    ast.parse(path.read_text(), filename=str(path))
PY
