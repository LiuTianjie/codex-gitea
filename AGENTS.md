# gitea-review-agent

## Production deployment

- Production source is this checkout. Commit the intended change first, then
  build from the working tree that contains that commit.
- Deploy with Luma local build, not GHCR `:latest` + `luma deploy`:

  ```bash
  luma build local . --platform linux/amd64 --timeout 3000
  ```

- `--platform linux/amd64` is required because the live service is pinned to
  the `lab` amd64 node. Do not let an arm64-only local image become
  authoritative.
- `luma.yaml` is gitignored. It defines service `codex-gitea`, Tailscale
  relay `codex-bot.itool.tech`, and the existing Docker volume names. Do not
  rename those volumes.
- Do not substitute Repository Import, an ad hoc `docker push`, GitHub Actions
  GHCR publish, or plain `luma deploy luma.yaml` against `ghcr.io/...:latest`.
- A rollout is complete only when Luma history shows a new stable version whose
  image is `local-<build-id>` (or that same digest), and both
  `https://codex-bot.itool.tech/healthz` and `/readyz` return `ok`.
