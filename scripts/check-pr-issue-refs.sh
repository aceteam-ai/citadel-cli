#!/bin/sh
# Fail when a PR names an issue (title "#N", or branch fix-N, fix/N-..., issue-N)
# but its body has neither a closing keyword for it (Closes/Fixes/Resolves #N)
# nor an explicit "Refs #N". Input comes from the environment, never argv:
#   PR_TITLE PR_BRANCH PR_BODY PR_BASE DEFAULT_BRANCH
# PRs whose base is not the default branch are exempt: GitHub only auto-closes
# issues on merges into the default branch.
# Exit 0 = ok/exempt, 1 = missing reference.

PR_TITLE=${PR_TITLE-}
PR_BRANCH=${PR_BRANCH-}
PR_BODY=${PR_BODY-}
PR_BASE=${PR_BASE-}
DEFAULT_BRANCH=${DEFAULT_BRANCH:-main}
# owner/repo of this repository; "Closes owner/repo#N" for it also counts.
PR_REPO=${PR_REPO-}

if [ -n "$PR_BASE" ] && [ "$PR_BASE" != "$DEFAULT_BRANCH" ]; then
  echo "base '$PR_BASE' is not the default branch '$DEFAULT_BRANCH' (stacked PR): exempt"
  exit 0
fi

# Bare same-repo "#N" in the title. Cross-repo refs (org/repo#N) are blanked
# first; "#" must not follow a word char, "/" or "." so versions like
# v2.247.0 (no '#') and cross-repo refs never count.
title_nums=$(printf '%s\n' "$PR_TITLE" |
  sed -E 's#[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+\#[0-9]+# #g' |
  grep -oE '(^|[^A-Za-z0-9_/.#-])#[0-9]+' |
  grep -oE '[0-9]+$')

# Branch names: fix-N, fix/N-slug, issue-N, ci/N-slug, ... at start or after '/'.
# Dates (docs/2026-11-21-notes) are blanked first so a year never reads as an issue.
branch_nums=$(printf '%s\n' "$PR_BRANCH" |
  sed -E 's#[0-9]{4}-[0-9]{1,2}-[0-9]{1,2}#date#g' |
  grep -oE '(^|/)(fix|fixes|issue|issues|bug|hotfix|feat|ci|chore|docs|test|refactor|perf)[-/_][0-9]+([^0-9]|$)' |
  grep -oE '[0-9]+')

nums=$(printf '%s\n%s\n' "$title_nums" "$branch_nums" | grep -E '^[0-9]+$' | sort -un)

body_lc=$(printf '%s\n' "$PR_BODY" | tr 'A-Z' 'a-z')
repo_re=""
if [ -n "$PR_REPO" ]; then
  repo_re="($(printf '%s' "$PR_REPO" | tr 'A-Z' 'a-z' | sed 's/[.]/[.]/g'))?"
fi
missing=""
for n in $nums; do
  if printf '%s\n' "$body_lc" |
    grep -Eq "(^|[^a-z0-9])(close[sd]?|fix(e[sd])?|resolve[sd]?|refs?):? +$repo_re#$n([^0-9]|\$)"; then
    continue
  fi
  missing="$missing #$n"
done

if [ -n "$missing" ]; then
  set -- $missing
  n=${1#\#}
  echo "PR names issue(s)$missing in its title/branch but the body has no closing keyword or Refs for them."
  echo "Fix: add 'Closes #$n' (or Fixes/Resolves) to the PR body, or 'Refs #$n' if it must not close the issue."
  exit 1
fi
exit 0
