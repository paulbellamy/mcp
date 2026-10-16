#!/bin/bash
# evaluate.sh BRANCH LABEL [REPS] — gate + bench a candidate branch (orchestrator only).
set -euo pipefail
BRANCH=$1; LABEL=$2; REPS=${3:-6}
REPO=/home/user/mcp
SP=${AR_SCRATCH:-/tmp/claude-0/-home-user/d00c192f-9989-5be5-80ac-d670b8d21ee6/scratchpad}
BASE=${AR_BEST:-autoresearch/agent-ux}
WT=$SP/wt/eval-$LABEL
mkdir -p $SP/bins $SP/evals $SP/wt
# 0. the scorer on disk must still match the hashes frozen at contract time
python3 -c "
import json,hashlib,sys
s=json.load(open('$REPO/.autoresearch/state.json'))
bad=[p for p,h in s['frozen'].items() if hashlib.sha256(open('$REPO/'+p,'rb').read()).hexdigest()!=h]
sys.exit('GATE FAIL: frozen files modified: %s' % bad if bad else 0)"
git -C $REPO diff --quiet HEAD -- .autoresearch/bench || { echo "GATE FAIL: uncommitted bench edits"; exit 2; }
# 1. frozen-set / scope check
BAD=$(git -C $REPO diff --name-only "$BASE...$BRANCH" | grep -vE '^(skills/mcp-cli/SKILL\.md|README\.md|[^/]+\.go)$' || true)
if [ -n "$BAD" ]; then echo "GATE FAIL: out-of-scope paths: $BAD"; exit 2; fi
# 2. build + vet + test + lint on a clean checkout of the candidate
rm -rf $WT; git -C $REPO worktree prune; git -C $REPO worktree add -q --detach $WT "$BRANCH"
cd $WT
go build -o $SP/bins/mcp-$LABEL . || { echo "GATE FAIL: build"; exit 2; }
go vet ./... || { echo "GATE FAIL: vet"; exit 2; }
go test ./... >$SP/evals/$LABEL.gotest.log 2>&1 || { tail -30 $SP/evals/$LABEL.gotest.log; echo "GATE FAIL: tests"; exit 2; }
golangci-lint run ./... || { echo "GATE FAIL: lint"; exit 2; }
git -C $REPO diff --stat "$BASE...$BRANCH" | tail -1
cd $REPO; git worktree remove --force $WT
# 3. bench (frozen scorer from the BEST branch's checkout, never the candidate's)
python3 $REPO/.autoresearch/bench/bench.py run --binary $SP/bins/mcp-$LABEL --out $SP/evals/$LABEL --reps $REPS --workers 10 > $SP/evals/$LABEL.log 2>&1
python3 $REPO/.autoresearch/tools/brief.py $SP/evals/$LABEL > $SP/evals/$LABEL.brief.md
python3 -c "import json;s=json.load(open('$SP/evals/$LABEL/summary.json'));print(json.dumps({k:s[k] for k in ('score','score_sd','pass_rate','mean_cost','total_cost','mean_bash_calls','mean_output_chars','per_cond')}))"
