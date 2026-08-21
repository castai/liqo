# Plan: Enable Helm chart release on every master commit

## Goal
Modify the existing CI workflow so that the Liqo Helm chart is packaged and published not only on version tags, but also on every push to `master` in the `castai/liqo` fork. This allows the omni-agent chart to consume a published fork chart for each commit without relying on the upstream chart.

## Constraints
- Keep tag-based releases unchanged (they are the official releases).
- Reuse the existing `release` job in `.github/workflows/integration.yml` and the chart-releaser tooling already in use.
- Do not introduce new secrets or external registries.
- Master commits must produce a unique, immutable chart version.

## Current behavior
- Workflow triggers on `push.tags: v*` and `push.branches: master`.
- The `configure` job already outputs `commit-ref=github.sha` for master pushes.
- The `release` job is gated by `if: github.event_name == 'push' && startsWith(github.ref, 'refs/tags/v')`, so it skips master.

## Proposed change

### Chunk 1: Update release job condition (simple)
**Files changed:**
- `.github/workflows/integration.yml`

**Change:**
Update the `release` job `if` condition to also run on master branch pushes:

```yaml
release:
  runs-on: ubuntu-latest
  needs: [build-standard, build-go, configure, liqoctl]
  if: github.event_name == 'push' && (startsWith(github.ref, 'refs/tags/v') || github.ref == 'refs/heads/master')
```

**Versioning behavior:**
- Tags: continue to use the tag value (e.g. `v0.10.0`) as chart version, release name, and app version.
- Master: uses the full commit SHA as chart version, release name, and app version (already provided by `needs.configure.outputs.commit-ref`).

**Acceptance criteria:**
- `release` job runs on the next master push after merge.
- `release` job still runs on the next `v*` tag push.
- Master-produced chart is downloadable from the GitHub release named after the commit SHA.
- Helm index (`index.yaml` on GitHub Pages) is updated with the new chart entry.

**Test strategy:**
- Validate workflow syntax with `actionlint` or `gh workflow lint` after editing.
- Monitor the next master push run to confirm the release job executes.
- Verify the chart package version matches the commit SHA.

**Complexity:** simple

## Potential issues and mitigations

1. **GitHub Releases noise**
   - Every master commit creates a GitHub Release. The releases list becomes noisy and may push real versioned releases out of the first page.
   - *Mitigation:* mark master releases as prerelease by setting `prerelease: true` in the `ncipollo/release-action` step when on master.

2. **Helm index growth**
   - Each master commit appends a new entry to `index.yaml` on the `gh-pages` branch. Over time the file grows, slowing `helm repo update`.
   - *Mitigation:* periodically prune old master entries, or accept the growth for a test fork and prune manually when it becomes a problem.

3. **Chart version is not semantic**
   - Commit SHAs are not valid SemVer. Helm tolerates non-SemVer versions but tools and consumers may warn or fail strict checks.
   - *Mitigation:* accept for test charts, or switch to a `0.0.0-<short-sha>` (SemVer with prerelease metadata) scheme. This would require updating the configure job for master to emit such a version.

4. **Index update races**
   - Multiple merges in quick succession can race when `cr index --push` fetches and pushes `index.yaml`.
   - *Mitigation:* GitHub serializes pushes to `gh-pages`, and `cr index --push` does a force push, but concurrent runs can still lose entries. Keep an eye on it; if it happens frequently, serialize release jobs with `concurrency`.

5. **Storage/artifact cost**
   - Each release uploads a `.tgz` chart package plus liqoctl archives. Frequent merges increase storage usage.
   - *Mitigation:* GitHub releases are free for public repos; for private repos monitor storage.

6. **Discoverability for testers**
   - Testers need to know the exact commit SHA to pin in the omni-agent chart.
   - *Mitigation:* expose the latest master chart version through a stable URL or document how to retrieve it from the GitHub Pages index.

7. **Fork drift vs upstream**
   - As noted in the conversation, the fork is drifting from upstream (e.g., virtual-kubelet changes). Testing with the fork chart is intentional and safer than upstream, but rebases may change which commits exist on `master`.
   - *Mitigation:* this is a process issue, not a technical one; keep rebases coordinated.

## Decision log
- **Reuse `release` job rather than a new workflow:** minimizes duplication and uses the existing build artifacts (liqoctl archives) and chart-releaser setup.
- **Use commit SHA for master versions:** already emitted by `configure`; no additional logic needed.
- **Do not add SemVer conversion in this change:** keeps the diff minimal. If strict SemVer becomes required, it can be added to the `configure` job later.
