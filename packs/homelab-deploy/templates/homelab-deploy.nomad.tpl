job "[[ var "job_name" . ]]" {
  datacenters = ["dc1"]
  type        = "service"
  namespace   = "[[ var "nomad_namespace" . ]]"
  node_pool   = "[[ var "nomad_node_pool" . ]]"

  group "homelab-deploy" {
    # One instance: sessions are in memory and there is a single job slot.
    # The single-node-writer volume also stops a second copy from starting.
    count = 1

    volume "work" {
      type            = "csi"
      source          = "[[ var "csi_volume_name" . ]]"
      access_mode     = "single-node-writer"
      attachment_mode = "file-system"
    }

    network {
      mode = "host"
      # Dynamic: only Traefik talks to this.
      port "http" {}
    }

    service {
      name     = "[[ var "job_name" . ]]"
      port     = "http"
      provider = "nomad"
      # TLS only. Login is done by the app itself (OIDC), so there is no
      # forward-auth middleware on purpose.
      tags = [
        "traefik.enable=true",
        "traefik.http.routers.[[ var "job_name" . ]].rule=Host(`[[ var "domain" . ]]`)",
        "traefik.http.routers.[[ var "job_name" . ]].tls=true",
        "traefik.http.routers.[[ var "job_name" . ]].tls.certresolver=letsencrypt",
        "traefik.http.routers.[[ var "job_name" . ]].priority=1",
      ]
      # TCP on purpose: without a session GET /auth/login is a 302 and
      # everything else a 401, and Nomad http checks only pass on 2xx.
      # Switch to http when the app has an unauthenticated /healthz.
      check {
        name     = "alive"
        type     = "tcp"
        interval = "10s"
        timeout  = "2s"
      }
    }
[[- if var "fix_volume_ownership" . ]]

    # A fresh CSI volume is usually root-owned; the image runs as uid 10001.
    task "volume-ownership" {
      driver = "podman"

      lifecycle {
        hook    = "prestart"
        sidecar = false
      }

      config {
        image      = "[[ var "image_repository" . ]]:[[ var "image_tag" . ]]"
        user       = "root"
        entrypoint = ["/bin/chown"]
        args       = ["10001:10001", "/work"]
      }

      volume_mount {
        volume      = "work"
        destination = "/work"
        read_only   = false
      }

      resources {
        cpu    = 50
        memory = 32
      }
    }
[[- end ]]

    task "homelab-deploy" {
      driver = "podman"

      vault {
        role = "[[ var "vault_access_role" . ]]"
      }

      config {
        image        = "[[ var "image_repository" . ]]:[[ var "image_tag" . ]]"
        network_mode = "host"
        # The image entrypoint is homelab-deploy; a shell wrapper is needed
        # only to sync the checkout before serving. exec keeps serve as the
        # main process so it receives the stop signal.
        entrypoint = ["/bin/sh", "-c"]
        args = [
          <<-EOT
          set -eu
[[- if var "deploy_repos" . ]]
          cp "$NOMAD_TASK_DIR/deploy-repos" /work/.deploy-repos
          homelab-deploy --env-file "$NOMAD_SECRETS_DIR/.env" -C /work sync || echo "sync failed: continuing with the existing checkout" >&2
[[- end ]]
          exec homelab-deploy --env-file "$NOMAD_SECRETS_DIR/.env" -C /work serve "$NOMAD_ADDR_http"
          EOT
        ]
      }

      logs {
        max_files     = 5
        max_file_size = 10
      }

      volume_mount {
        volume      = "work"
        destination = "/work"
        read_only   = false
      }

      # Not secret. The three group variables map Authentik groups to roles.
      env {
        HOMELAB_DEPLOY_OIDC_ISSUER             = "[[ var "oidc_issuer" . ]]"
        HOMELAB_DEPLOY_OIDC_CLIENT_ID          = "[[ var "oidc_client_id" . ]]"
        HOMELAB_DEPLOY_OIDC_CLIENT_SECRET_FILE = "/secrets/oidc_client_secret"
        HOMELAB_DEPLOY_PUBLIC_URL              = "https://[[ var "domain" . ]]"
        HOMELAB_DEPLOY_VIEWER_GROUPS           = "[[ var "viewer_groups" . ]]"
        HOMELAB_DEPLOY_PLANNER_GROUPS          = "[[ var "planner_groups" . ]]"
        HOMELAB_DEPLOY_APPLIER_GROUPS          = "[[ var "applier_groups" . ]]"
[[- if var "oidc_groups_claim" . ]]
        HOMELAB_DEPLOY_OIDC_GROUPS_CLAIM       = "[[ var "oidc_groups_claim" . ]]"
[[- end ]]
[[- if var "oidc_username_claim" . ]]
        HOMELAB_DEPLOY_OIDC_USERNAME_CLAIM     = "[[ var "oidc_username_claim" . ]]"
[[- end ]]
[[- if var "oidc_scopes" . ]]
        HOMELAB_DEPLOY_OIDC_SCOPES             = "[[ var "oidc_scopes" . ]]"
[[- end ]]
[[- if var "session_ttl" . ]]
        HOMELAB_DEPLOY_SESSION_TTL             = "[[ var "session_ttl" . ]]"
[[- end ]]
[[- if var "git_ssh_key_enabled" . ]]
        GIT_SSH_COMMAND                        = "ssh -i /secrets/git_ssh_key -o IdentitiesOnly=yes -o UserKnownHostsFile=/local/known_hosts -o StrictHostKeyChecking=yes"
[[- end ]]
      }

      # The terraform .env ([tfvars] / [env] sections, backend credentials).
      # Rendered under secrets/ (tmpfs), never onto the volume; passed with
      # --env-file. .env.shared stays in the repo on the volume.
      template {
        destination = "secrets/.env"
        perms       = "0400"
        uid         = 10001
        gid         = 10001
        data        = <<-EOH
        {{- with secret "[[ var "vault_secret_path" . ]]" }}{{ .Data.data.ENV }}{{ end }}
        EOH
      }

      # No trailing newline: -}} trims the heredoc's final newline.
      template {
        destination = "secrets/oidc_client_secret"
        perms       = "0400"
        uid         = 10001
        gid         = 10001
        data        = <<-EOH
        {{- with secret "[[ var "vault_secret_path" . ]]" }}{{ .Data.data.OIDC_CLIENT_SECRET }}{{ end -}}
        EOH
      }
[[- if var "deploy_repos" . ]]

      # Plain heredoc below: a multi-line value must not be re-indented.
      template {
        destination = "local/deploy-repos"
        data        = <<EOH
[[ var "deploy_repos" . ]]
EOH
      }
[[- end ]]
[[- if var "git_ssh_key_enabled" . ]]

      template {
        destination = "secrets/git_ssh_key"
        perms       = "0400"
        uid         = 10001
        gid         = 10001
        data        = <<-EOH
        {{- with secret "[[ var "vault_secret_path" . ]]" }}{{ .Data.data.GIT_SSH_KEY }}{{ end }}
        EOH
      }

      template {
        destination = "local/known_hosts"
        data        = <<EOH
[[ var "git_known_hosts" . ]]
EOH
      }
[[- end ]]

      resources {
        cpu    = [[ var "task_resources_cpu" . ]]
        memory = [[ var "task_resources_memory" . ]]
      }
    }
  }
}
