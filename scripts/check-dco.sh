#!/usr/bin/env bash
# Enforce the Developer Certificate of Origin sign-off (ADR-0001, CLAUDE.md): every commit a
# branch adds must carry a "Signed-off-by:" trailer, which `git commit -s` writes.
#
# The check is scoped to the commits the branch INTRODUCES, and never to all of history. A
# trailer cannot be added to a commit that is already on main without rewriting it, so a
# whole-history check would fail permanently on the commits that predate this script — and a
# check nobody can make pass is a check people learn to ignore. GitHub's squash merge copies the
# trailer from the branch commits into the squash commit, so checking the branch is what puts
# the trailer on main.
#
# The base is worked out differently depending on who is calling:
#
#   - CI knows it exactly. On a pull request the checked-out commit is a merge of the branch and
#     the base branch that GitHub synthesises for the run, so `origin/main..HEAD` describes
#     something nobody wrote. The workflow passes the pull request's own base and head commits in
#     DCO_BASE and DCO_HEAD instead.
#   - Locally there is no pull request, so the base is where the current branch diverged from
#     origin/main: the merge base. If that cannot be worked out, the check degrades to a no-op
#     rather than an error — see below.
#
# Run from the repo root. Exits non-zero on any violation.
set -euo pipefail

# A skip is a pass. This check reads history that a working copy is not obliged to have: a
# shallow clone, a clone with no `origin`, a detached HEAD with no shared ancestry. None of those
# say anything about whether commits are signed off, and failing `task check` on a developer's
# machine for one of them would only teach them to skip the gate. CI passes an explicit base, so
# the case that actually gates a merge cannot fall into this branch silently.
skip() {
  echo "DCO check skipped: $1."
  exit 0
}

base="${DCO_BASE:-}"
head="${DCO_HEAD:-HEAD}"

git rev-parse --git-dir >/dev/null 2>&1 || skip "not a git repository"

if [[ -z "$base" ]]; then
  if ! base="$(git merge-base origin/main "$head" 2>/dev/null)"; then
    skip "cannot find a merge base with origin/main (shallow clone, no origin, or unrelated history)"
  fi
fi

git rev-parse --quiet --verify "${base}^{commit}" >/dev/null 2>&1 ||
  skip "base commit ${base} is not present in this clone"
git rev-parse --quiet --verify "${head}^{commit}" >/dev/null 2>&1 ||
  skip "head commit ${head} is not present in this clone"

# --no-merges EXEMPTS merge commits, for two reasons. A merge commit contributes no changes of
# its own — everything it carries reaches it through a parent, and those commits are checked
# here. And on a pull request the commit CI checks out IS a merge commit, synthesised by GitHub
# from the branch and the base branch; no author ever sees it, let alone signs it off. main is
# required to be linear, so a merge commit does not land there either way.
#
# Bot and automation commits are NOT exempt, and there is deliberately no author allowlist. The
# sign-off is a statement by the person contributing a change that they have the right to
# contribute it; making the commit from a script does not remove the statement or the person.
# An allowlist would also be worthless as a rule, because the author field is chosen freely by
# whoever writes the commit — it would create the one way around the check and secure nothing.
# Automation that commits to this repo passes `-s` like everybody else.
fail=0
missing=0
checked=0

while IFS= read -r sha; do
  [[ -n "$sha" ]] || continue
  checked=$((checked + 1))
  # Matched against the whole message rather than via `git interpret-trailers`, so a sign-off
  # still counts when a later line (a co-author, a revert reference) follows it. Keys are
  # case-insensitive, and the value has to look like "Name <email>" so an empty trailer does not
  # pass for one.
  if git show -s --format=%B "$sha" | grep -Eqi '^Signed-off-by:[[:space:]]+.+<.+@.+>'; then
    continue
  fi
  echo "MISSING Signed-off-by: $(git show -s --format='%h %s' "$sha")"
  missing=$((missing + 1))
  fail=1
done < <(git rev-list --no-merges "${base}..${head}")

if [[ "$fail" -ne 0 ]]; then
  {
    echo
    echo "DCO check failed: ${missing} commit(s) added by this branch have no Signed-off-by trailer (${checked} checked)."
    echo "Every commit needs one for provenance (ADR-0001). \`git commit -s\` writes it."
    echo
    if [[ "$missing" -eq 1 && "$checked" -eq 1 ]]; then
      echo "There is one commit and it is the tip, so amend it:"
      echo "    git commit --amend -s --no-edit"
    else
      echo "If the only commit listed above is the most recent one, amend it:"
      echo "    git commit --amend -s --no-edit"
      echo
      echo "If more than one is listed, or the one listed is not the tip, sign off every commit"
      echo "back to the base:"
      echo "    git rebase --signoff ${base}"
    fi
    echo
    echo "Both rewrite commits, so the branch needs \`git push --force-with-lease\` afterwards,"
    echo "and \`git config commit.gpgsign true\` should be set first or the rewrite drops the"
    echo "signatures that main requires."
  } >&2
  exit 1
fi

if [[ "$checked" -eq 0 ]]; then
  echo "DCO check passed: this branch adds no commits over ${base}."
else
  echo "DCO check passed (${checked} commit(s) added by this branch carry a sign-off)."
fi
