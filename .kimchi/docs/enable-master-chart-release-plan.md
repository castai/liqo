# Plan: Release Helm chart on every branch push (no GitHub Release)

## Goal
Modify the castai/liqo fork CI so the Helm chart is published for every push to **any branch**, not just `master`, **without creating a GitHub Release or git tag**. Tagged releases (`v*`) keep their current behavior.

## Constraints
- No GitHub Release for branch commits.
- No git tag for branch commits.
- Chart versions must be valid SemVer.
- Tagged releases remain unchanged.

## Current state
- Workflow file: `.github/workflows/integration.yml`
- Trigger was limited to `master` and `v*` tags.
- `release` job ran only on tags and `master`.
- Branch chart versions used `0.0.0-master.<short-sha>` for `master`.

## Proposed change
1. Change push trigger to all branches: `branches: ["**"]`.
2. Change `release` job condition to run on any branch push (`refs/heads/**`).
3. Compute chart version dynamically:
   - Tags: use tag name (e.g., `v0.10.0`).
   - Branches: sanitize branch name and use `0.0.0-<sanitized-branch>.<short-sha>`.
4. Publish branch charts to `gh-pages` with a branch-aware commit message.

## Files changed
- `.github/workflows/integration.yml`

## Acceptance criteria
- `v*` tag pushes create a GitHub Release and update `index.yaml` as before.
- Any branch push packages the chart and adds it to the `gh-pages` `index.yaml`.
- No release or tag is created for branch pushes.
- Branch names with slashes or special characters are sanitized into valid SemVer prerelease identifiers.

## Risks
1. **CI cost**: the full pipeline (build-standard, build-go, liqoctl, release) now runs on every branch push.
2. **Index bloat**: every branch push appends a new entry to `index.yaml` and a new `.tgz` to `gh-pages`.
3. **Default install behavior**: `helm install` without `--version` still picks the latest stable tag, ignoring branch charts.
4. **Branch name collisions**: different branches could sanitize to the same prefix, but the short SHA keeps the full version unique.
