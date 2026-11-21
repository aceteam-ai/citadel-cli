#!/bin/sh
# Table tests for check-pr-issue-refs.sh. Run: sh scripts/test-check-pr-issue-refs.sh
dir=$(cd "$(dirname "$0")" && pwd)
fails=0
# t NAME EXPECTED_EXIT TITLE BRANCH BODY [BASE]
t() {
  PR_TITLE=$3 PR_BRANCH=$4 PR_BODY=$5 PR_BASE=${6:-main} DEFAULT_BRANCH=main \
    sh "$dir/check-pr-issue-refs.sh" >/dev/null 2>&1
  got=$?
  if [ "$got" -ne "$2" ]; then echo "FAIL: $1 (want $2 got $got)"; fails=$((fails+1)); else echo "ok:   $1"; fi
}
t "title #N no keyword"        1 "feat(x): thing (#123)" "feat/thing" "does stuff"
t "title #N Closes"            0 "feat(x): thing (#123)" "feat/thing" "Closes #123"
t "Fixes lowercase"            0 "thing (#123)" "x" "fixes #123"
t "Resolved mixed case"        0 "thing (#123)" "x" "RESOLVED #123"
t "Closes with colon"          0 "thing (#123)" "x" "Closes: #123"
t "Refs explicit"              0 "thing (#123)" "x" "Refs #123"
t "keyword for other issue"    1 "thing (#123)" "x" "Closes #124"
t "prefix issue #12 vs #123"   1 "thing (#12)" "x" "Closes #123"
t "multiple, one missing"      1 "a (#1) b (#2)" "x" "Closes #1"
t "multiple, all present"      0 "a (#1) b (#2)" "x" "Closes #1
Refs #2"
t "#N only in body"            0 "plain title" "feat/plain" "see #55"
t "cross-repo in title"        0 "bump org/repo#123 pin" "x" "nothing"
t "cross-repo with dots"       0 "bump aceteam-ai/aceteam#123" "x" ""
t "version string"             0 "chore(release): bump VERSION to 2.247.0" "x" ""
t "version with v"             0 "release v2.247.0 (hotfix)" "x" ""
t "branch fix-N"               1 "no number" "fix-77" "body"
t "branch fix-N ok"            0 "no number" "fix-77" "Fixes #77"
t "branch fix/N-slug"          1 "t" "fix/88-slug" "body"
t "branch issue-N"             1 "t" "issue-99" "body"
t "branch issue-N ok"          0 "t" "issue-99" "Closes #99"
t "branch ci/N-slug"           1 "t" "ci/1868-issue-ref-check" ""
t "branch no issue"            0 "t" "release-2" ""
t "title+branch same num"      0 "t (#5)" "fix-5" "Closes #5"
t "stacked base exempt"        0 "thing (#123)" "x" "nothing" "feat/parent"
t "default base enforced"      1 "thing (#123)" "x" "nothing" "main"
t "empty everything"           0 "" "" ""
[ "$fails" -eq 0 ] && echo "all passed" || { echo "$fails failed"; exit 1; }
