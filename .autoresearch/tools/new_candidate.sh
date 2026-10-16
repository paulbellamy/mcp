#!/bin/bash
# new_candidate.sh RUN TAG [BASE] — sparse worktree (no .autoresearch/) on branch autoresearch/RUN-TAG cut from BASE.
set -euo pipefail
RUN=$1; TAG=$2; BASE=${3:-autoresearch/agent-ux}
SP=${AR_SCRATCH:-/tmp/claude-0/-home-user/d00c192f-9989-5be5-80ac-d670b8d21ee6/scratchpad}
WT=$SP/wt/$RUN-$TAG; BR=autoresearch/$RUN-$TAG
git -C /home/user/mcp worktree add -q --no-checkout -b $BR $WT $BASE
cd $WT && git sparse-checkout set --no-cone '/*' '!/.autoresearch/' && git checkout -q
echo "$WT $BR"
