# patched fork — upstream Alpha + group failover patch

This fork of [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) keeps a
patched build available while staying trivially rebaseable onto upstream.

## Branch layout

| Branch | Content | Kept up to date by |
|---|---|---|
| **`patched`** (default branch) | `Alpha` + one commit (patch + this fork's CI/docs) | the `sync-upstream` workflow: daily cron, or one click in **Actions → sync-upstream → Run workflow** |
| `Alpha` | plain mirror of `MetaCubeX/mihomo:Alpha` | GitHub's **Sync fork** button (branch page) |
| `main` | inherited from upstream, unrelated legacy tree — ignored | upstream |

Upstream's default branch is `main`, but the Go proxy code lives on **`Alpha`**
(upstream's `main` is an unrelated legacy tree). `patched` is based on `Alpha`.

### Why the "Sync fork" button cannot drive `patched`

GitHub syncs `fork:<name>` from `upstream:<name>` — the names have to match.
`patched` does not exist upstream, and in that case GitHub falls back to
comparing with the upstream *default* branch, which here is the unrelated
`main` tree. Measured against this fork:

```console
$ curl -X POST .../repos/<owner>/mihomo/merge-upstream -d '{"branch":"omp"}'      # any fork-only name
{"message": "There are merge conflicts", "status": "409"}

$ curl -X POST .../repos/<owner>/mihomo/merge-upstream -d '{"branch":"Alpha"}'    # same name as upstream
{"message": "This branch is not behind the upstream MetaCubeX:Alpha.", "merge_type": "none"}
```

So:

* to sync **`Alpha`** (the mirror) — use the button, it does exactly the right thing;
* to sync **`patched`** — use `sync-upstream` (one click in the Actions tab, or
  `gh workflow run sync-upstream.yml --repo <owner>/mihomo`). It rebases the one
  patch commit onto the current upstream `Alpha`, force-pushes, and only then
  triggers a rebuild. This is strictly better than a merge-based button sync:
  the branch stays "upstream + exactly one commit", and the fork infrastructure
  (workflows, patch copy, this file) survives every sync.

## What the patch adds

`patches/patched-group-failover.patch` (touches only `adapter/outboundgroup/`):

* `race: N` — dial the first N healthy nodes concurrently, keep the first
  success, so a dead or hanging node no longer costs a full dial timeout
* `retry: N` — sequential retry on the next healthy node (for nodes that fail
  fast instead of hanging)
* `warm-standbys: N` / `warm-interval: S` — keep N standbys pre-connected
  (traffic-triggered and rate limited, no background goroutine)
* when every node is marked dead, the race slots are filled with dead nodes, so
  a recovered node is used by the next connection instead of waiting for a
  health check
* an explicitly pinned node (`Set` / `default-selected`) disables racing for
  that group, so operator choices are never overruled

```yaml
proxy-groups:
  - name: ovpp_Any
    type: url-test
    use: [ovpp]
    url: "https://cp.cloudflare.com/"
    expected-status: 204
    interval: 60
    race: 2
    warm-standbys: 2
    warm-interval: 30
```

## Keeping up with upstream

```sh
# patched branch: rebase onto upstream Alpha + rebuild (daily cron does this too)
gh workflow run sync-upstream.yml --repo <owner>/mihomo
# upstream did not move but you want fresh binaries anyway
gh workflow run sync-upstream.yml --repo <owner>/mihomo -f force_build=true
```

Follow the stable Go line instead of `Alpha` with one repository variable
(the patch applies to `Meta` too):

```sh
gh variable set BASE_BRANCH --body Meta --repo <owner>/mihomo
```

If the rebase conflicts (upstream touched the same code) `patched` is left
untouched, an issue is opened, and the workflow fails — nothing stale is ever
published. Fix it by hand:

```sh
git clone --branch patched git@github.com:<owner>/mihomo.git
cd mihomo
git remote add upstream https://github.com/MetaCubeX/mihomo.git
git fetch upstream Alpha
git rebase FETCH_HEAD              # resolve conflicts in adapter/outboundgroup/
git push --force-with-lease origin patched
# then refresh the stored patch
git diff upstream/Alpha...HEAD -- adapter/outboundgroup/ > patches/patched-group-failover.patch
```

## Using the build

Every push to `patched` (and every sync) republishes the rolling pre-release
**`patched-latest`**:

```
https://github.com/<owner>/mihomo/releases/download/patched-latest/mihomo-linux-amd64.gz
https://github.com/<owner>/mihomo/releases/download/patched-latest/mihomo-linux-arm64.gz
```

Each asset has a `.sha256` next to it and the release carries `version.txt`
(patch commit + upstream base + build time). In a Dockerfile:

```dockerfile
ARG hostarch=amd64
RUN curl -fsSL "https://github.com/<owner>/mihomo/releases/download/patched-latest/mihomo-linux-${hostarch}.gz" \
      | gzip -d > /bin/mihomo && chmod +x /bin/mihomo
```

The binary names both commits:

```sh
mihomo -v     # patched-<patch-sha>+upstream-<upstream-base-sha>
```

Build it yourself instead:

```sh
git clone --branch patched https://github.com/<owner>/mihomo.git
cd mihomo
CGO_ENABLED=0 go build -tags with_gvisor -trimpath \
  -ldflags "-X 'github.com/metacubex/mihomo/constant.Version=patched'" -o mihomo .
```
