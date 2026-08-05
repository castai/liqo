# ci: publish Helm charts for every master commit without GitHub Releases

## What
Updates the `release` job in `.github/workflows/integration.yml` so that the Helm chart is published for every push to `master`, in addition to the existing `v*` tag releases. Master builds do **not** create a GitHub Release or git tag.

## Why
Today, testing upstream changes rebased onto the fork requires cutting a tag or a release. This makes it possible to install a chart built from the latest `master` commit directly from the Helm repository, simply by referencing its SemVer prerelease version.

## How
- The workflow trigger remains `branches: [master]`.
- The `release` job runs on `refs/heads/master` in addition to `refs/tags/v*`.
- Chart versions are computed dynamically:
  - Tags: `v0.x.x` (unchanged).
  - Master: `0.0.0-master.<short-sha>`.
- Master charts are packaged and pushed directly to the `gh-pages` branch into a `charts/` directory, and `index.yaml` is regenerated with `helm repo index ... --merge`.
- For master builds, `.Values.tag` is set to the commit SHA so the chart pulls images from the `-ci` suffixed repositories (`us-docker.pkg.dev/castai-hub/library/liqo/<component>-ci:<sha>`).
- The GitHub token is provided to git via the credential cache helper rather than embedding it in the remote URL.
- The GitHub Pages chart URL is computed from `GITHUB_REPOSITORY` instead of being hardcoded to `castai/liqo`.
- Tag releases keep using `ncipollo/release-action` + `cr index` as before.
- Tag-only steps (`cr` download, liqoctl artifact download, GitHub Releases, `cr index`) are skipped on master pushes.
- A final `Chart release info` step prints the chart version, ref, commit, and install commands to the job summary.

## Versioning example
```yaml
# index.yaml
entries:
  liqo:
    - version: 0.10.0
      urls:
        - https://github.com/castai/liqo/releases/download/v0.10.0/liqo-0.10.0.tgz
    - version: 0.0.0-master.abc123d
      urls:
        - https://castai.github.io/liqo/charts/liqo-0.0.0-master.abc123d.tgz
```

## Usage
```bash
# Install the latest stable release (default)
helm install omni-agent castai-liqo/liqo

# Install a specific master commit chart
helm install omni-agent castai-liqo/liqo --version 0.0.0-master.abc123d

# List all versions including master charts
helm search repo castai-liqo/liqo --versions --devel
```

## Verification
- Validated workflow YAML with `python3 -c 'import yaml; yaml.safe_load(...)'`.
- Ran `actionlint`; only pre-existing info-level `SC2086` warnings in the `configure` job remain.
- Actual chart publishing can only be verified by pushing to `master` or tagging.

## Image tags for master charts
The master chart version is `0.0.0-master.<short-sha>`, but the images referenced by the chart use the full commit SHA as the tag and the `-ci` repository suffix. For example:

```
us-docker.pkg.dev/castai-hub/library/liqo/liqo-controller-manager-ci:abc123def456...
```

This matches what the `build-standard` and `build-go` jobs push for non-tag commits.

## Potential issues / follow-ups
1. **Index/storage growth**: every master push appends a chart entry and `.tgz` to `gh-pages`. May need periodic cleanup.
2. **Default installs still pick stable tags**: `helm install` without `--version` ignores prerelease master charts.
3. **Push race condition**: concurrent master pushes to `gh-pages` could non-fast-forward fail; the next push recovers.
