# homelab-deploy pack

Runs the `homelab-deploy serve` web dashboard (plan / apply from a browser)
as one Nomad job on the podman driver. Login is done by the app itself
(OIDC, viewer / planner / applier roles), so the Traefik route is TLS only
with no forward-auth middleware.

The pack lives next to the code so one git tag pins both: use the same
release for the pack `ref` and for `image_tag` (`v0.2.0` -> `0.2.0`). Needs a
release that contains the OIDC login (v0.2.0 or later); older releases serve
without auth.

This repo is public: keep real hostnames, group names, Vault paths and
volume names in the stack that calls the pack, not here.

## What it runs

- One group, `count = 1` (sessions are in memory, one job slot). The
  `single-node-writer` CSI volume also prevents a second copy.
- Image `ghcr.io/byteford/deploy:<image_tag>` (public). Runs as uid 10001,
  working dir `/work`.
- Optional prestart task `volume-ownership` (default on): `chown 10001:10001 /work`
  as root, because a fresh CSI volume is usually root-owned.
- Main task starts `homelab-deploy --env-file /secrets/.env -C /work serve
  $NOMAD_ADDR_http`, bound to the allocation's node address and dynamic port,
  not all interfaces. With `deploy_repos` set it first copies the manifest
  to `/work/.deploy-repos` and runs `sync`; a failed sync is logged and the
  dashboard still starts on the existing checkout.
- TCP health check. Without a session `GET /auth/login` is a 302 and
  everything else a 401, and Nomad HTTP checks only pass on 2xx. Switch to
  HTTP when the app has an unauthenticated `/healthz`.

## Secrets (Vault, KV v2 at `vault_secret_path`)

Rendered by Nomad templates under the task's `secrets/` dir (tmpfs), never
onto the volume and never into the repo.

- `ENV`: the full contents of the terraform `.env` (`[tfvars]` / `[env]`
  sections: `TF_VAR_*`, backend credentials, provider tokens).
  `.env.shared` (non-secret) stays in the repo on the volume.
- `OIDC_CLIENT_SECRET`: confidential client secret, read via
  `HOMELAB_DEPLOY_OIDC_CLIENT_SECRET_FILE`.
- `GIT_SSH_KEY`: only with `git_ssh_key_enabled`. Use a read-only deploy key.
  Host checking is strict against `git_known_hosts`.

## One-off steps outside this pack (not automated)

1. Create the CSI volume (`csi_volume_name`).
2. Write the Vault secret fields above and a Vault role/policy that can read it
   (`vault_access_role`).
3. Create the OIDC application at the provider: confidential client, auth
   code grant, redirect URI `https://<domain>/auth/callback` (lower-case, no
   port), ID token carrying a top-level groups claim.
4. If using SSH sync: create the deploy key(s) on the repos.
5. Deploy this job outside any stack that homelab-deploy itself applies,
   otherwise an apply would restart the dashboard mid-run.

## Security notes

- `planner` is not read-only: Terraform runs provider code at plan time, so
  anyone who can change the planned repos can run code with this job's
  credentials. See the app README ("Authentication and roles").
- The app speaks plain HTTP; the hop from Traefik must stay on a private
  network. The pack binds to the node address, not 0.0.0.0, but that is still
  the LAN.
- Do not redeploy the job while a run is in progress: the process is killed
  and a remote-state lock may be left behind.
- The image's `terraform` is whatever was latest when that tag was built.
  The tag is immutable by convention only; pin by digest in the stack if you
  want it enforced.

## Variables

See `variables.hcl`. Required: `image_tag`, `domain`, `csi_volume_name`,
`vault_access_role`, `oidc_issuer`, `oidc_client_id`. At least one of the
three `*_groups` must be set or the app exits at start (it fails closed).

## Not verified

Rendered with a throwaway script and `terraform fmt` for syntax only, never
with `nomad-pack` or against a Nomad cluster. Unproven until a first run:
template `uid`/`gid` with the podman driver, the `entrypoint` override,
`$NOMAD_*` variables inside the container, and memory sizing.
