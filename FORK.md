# fork — upstream `Alpha` + group failover patch

This fork of [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) keeps a
patched build available while staying one click away from upstream.

## Branch layout

| Branch | Content | Synced by |
|---|---|---|
| **`Alpha`** (default branch) | upstream `Alpha` + one patch commit (plus this fork's CI/docs) | GitHub **Sync fork** button *or* the `sync-upstream` workflow |
| `main` | inherited from upstream, unrelated legacy tree — ignored | upstream |

Upstream's default branch is `main`, but that is an **unrelated legacy tree**;
the Go proxy code is on `Alpha`. The branch is deliberately called `Alpha` so
the Sync-fork button matches `MetaCubeX:Alpha` (GitHub syncs `fork:<name>` from
`upstream:<name>`; a fork-only name would be compared against upstream's
default branch instead — measured: `POST /merge-upstream {"branch":"<fork-only>"}`
→ `409 There are merge conflicts`).

## Syncing with upstream

Either path produces the same result; both only ever add a merge commit.

```text
# 1) GitHub UI: branch Alpha -> "Sync fork"      (merge of MetaCubeX:Alpha)
# 2) CI (daily cron, or one click in the Actions tab):
gh workflow run sync-upstream.yml --repo <owner>/mihomo
# upstream did not move but you want fresh binaries anyway:
gh workflow run sync-upstream.yml --repo <owner>/mihomo -f force_build=true
```

The workflow additionally fails loudly (issue + red run, no rebuild of stale
code) when upstream touched the same lines: `Alpha` is left untouched and the
conflict is reported instead of being pushed. Fix it by hand:

```sh
git clone --branch Alpha git@github.com:<owner>/mihomo.git
cd mihomo
git remote add upstream https://github.com/MetaCubeX/mihomo.git
git fetch upstream Alpha
git merge FETCH_HEAD        # resolve conflicts in adapter/outboundgroup/
git push origin Alpha
git diff upstream/Alpha...HEAD -- adapter/outboundgroup/ > patches/patched-group-failover.patch
```

Track the stable Go line instead of `Alpha` with one repository variable (the
patch also applies to `Meta`):

```sh
gh variable set BASE_BRANCH --body Meta --repo <owner>/mihomo
```

## What the patch adds

`patches/patched-group-failover.patch` (touches only `adapter/outboundgroup/`):

* `race: N` — dial the first N healthy nodes concurrently, keep the first
  success, so a dead or hanging node no longer costs a full dial timeout
* `retry: N` — sequential retry on the next healthy node (for nodes that fail
  fast instead of hanging)
* `warm-standbys: N` / `warm-interval: S` — keep N standbys pre-connected
  (traffic-triggered and rate limited, no background goroutine)
* when every node is marked dead, the race slots are filled with dead nodes, so
  a recovered node is picked up by the next connection
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

## Using the build

Every push to `Alpha` (and every sync) republishes the rolling pre-release
**`patched-latest`**:

```
https://github.com/<owner>/mihomo/releases/download/patched-latest/mihomo-linux-amd64.gz
https://github.com/<owner>/mihomo/releases/download/patched-latest/mihomo-linux-arm64.gz
```

Each asset has a `.sha256` next to it; `version.txt` records the build commit,
the upstream base it was merged with and the patch commit. In a Dockerfile:

```dockerfile
ARG hostarch=amd64
RUN curl -fsSL "https://github.com/<owner>/mihomo/releases/download/patched-latest/mihomo-linux-${hostarch}.gz" \
      | gzip -d > /bin/mihomo && chmod +x /bin/mihomo
```

The binary names both commits:

```sh
mihomo -v     # patched-<build-sha>+upstream-<upstream-base-sha>
```

Build it yourself instead:

```sh
git clone --branch Alpha https://github.com/<owner>/mihomo.git
cd mihomo
CGO_ENABLED=0 go build -tags with_gvisor -trimpath \
  -ldflags "-X 'github.com/metacubex/mihomo/constant.Version=patched'" -o mihomo .
```
