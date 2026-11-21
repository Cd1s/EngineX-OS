#!/usr/bin/env bash
# The one test command for lanes, reviewers and the merge train. See `tools/dev/test.sh --help`.
#   tools/dev/test.sh              targeted: changed files -> touched Go packages + importers, mapped Python/shell tests
#   tools/dev/test.sh --full       everything (fast tier on the x86 host, ARM tier on a build host, at the same time)
#   tools/dev/test.sh --no-cache   go test -count=1 (release candidates)
set -euo pipefail
[[ -r /etc/profile.d/buildtools.sh ]] && . /etc/profile.d/buildtools.sh
exec python3 "$(dirname "${BASH_SOURCE[0]}")/devtest.py" "$@"
