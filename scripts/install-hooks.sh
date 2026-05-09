#!/usr/bin/env bash
# Install Harbor's git hooks into .git/hooks/.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"

if [ ! -d .git ]; then
    echo "install-hooks: not a git repository (.git missing)"
    exit 1
fi

mkdir -p .git/hooks
cp scripts/hooks/pre-commit .git/hooks/pre-commit
chmod +x .git/hooks/pre-commit
echo "install-hooks: pre-commit installed to .git/hooks/pre-commit"
