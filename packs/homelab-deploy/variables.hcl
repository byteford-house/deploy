variable "nomad_node_pool" {
  type        = string
  description = "Node pool to deploy the job on."
  default     = "all"
}

variable "nomad_namespace" {
  type        = string
  description = "Namespace to deploy the job in."
  default     = "default"
}

variable "job_name" {
  type        = string
  description = "Name for the job and its Nomad service."
  default     = "homelab-deploy"
}

# --- Image ---

variable "image_repository" {
  type        = string
  description = "Image repository. The release workflow publishes ghcr.io/byteford/deploy (public)."
  default     = "ghcr.io/byteford/deploy"
}

variable "image_tag" {
  type        = string
  description = "Image tag, which is the release tag without the leading v (v0.2.0 -> 0.2.0). Required, no default: set it to the same release as the pack ref. Never latest: it moves, and so does the Terraform inside it."
}

# --- Network ---

variable "domain" {
  type        = string
  description = "External hostname. Used for the Traefik router and for HOMELAB_DEPLOY_PUBLIC_URL (https://<domain>). Register https://<domain>/auth/callback as the redirect URI at the OIDC provider."
}

# --- Storage ---

variable "csi_volume_name" {
  type        = string
  description = "Existing CSI volume mounted at /work: the Terraform repo (or .deploy-repos checkouts), local state if no remote backend, and .deploy-logs/."
}

variable "fix_volume_ownership" {
  type        = bool
  description = "Run a short prestart task as root that chowns /work to uid 10001 (the image user). Turn off if the volume cannot be chowned (e.g. NFS root_squash) and is already writable by 10001."
  default     = true
}

# --- Vault ---

variable "vault_access_role" {
  type        = string
  description = "Vault role (nomad-vault-access module) the job uses via workload identity."
}

variable "vault_secret_path" {
  type        = string
  description = "Vault KV v2 read path (with /data/). Fields: ENV (full contents of the .env file), OIDC_CLIENT_SECRET, and GIT_SSH_KEY when git_ssh_key_enabled."
  default     = "secret/data/application/homelab-deploy"
}

# --- OIDC (HOMELAB_DEPLOY_*) ---

variable "oidc_issuer" {
  type        = string
  description = "Issuer URL exactly as in the iss claim. Authentik: https://<auth host>/application/o/<app-slug>/ (keep the trailing slash)."
}

variable "oidc_client_id" {
  type        = string
  description = "OIDC client id of a confidential client. The secret comes from Vault (OIDC_CLIENT_SECRET)."
}

variable "viewer_groups" {
  type        = string
  description = "Comma-separated group names mapped to the viewer role. At least one of the three group variables must be set or the app refuses to start."
  default     = ""
}

variable "planner_groups" {
  type        = string
  description = "Comma-separated group names mapped to the planner role (plan, init, sync). Planners can run code from the repos through Terraform: see the README."
  default     = ""
}

variable "applier_groups" {
  type        = string
  description = "Comma-separated group names mapped to the applier role (apply)."
  default     = ""
}

variable "oidc_groups_claim" {
  type        = string
  description = "ID-token claim holding the groups. Empty = app default (groups)."
  default     = ""
}

variable "oidc_username_claim" {
  type        = string
  description = "Claim used as the user name in logs. Empty = app default (preferred_username, then email, then sub)."
  default     = ""
}

variable "oidc_scopes" {
  type        = string
  description = "Space or comma separated scopes. Empty = app default (openid profile)."
  default     = ""
}

variable "session_ttl" {
  type        = string
  description = "Session lifetime, e.g. 8h. Empty = app default (8h). Roles are read at login, so this is also how long a group change can take to apply."
  default     = ""
}

# --- Repo checkout and git over SSH (both optional, off by default) ---

variable "deploy_repos" {
  type        = string
  description = "Contents of a .deploy-repos manifest (multi-repo mode). When set, it is written to /work/.deploy-repos and `sync` runs before serve on every start (a failed sync is logged and does not stop the dashboard). Empty = the pack does not manage the checkout."
  default     = ""
}

variable "git_ssh_key_enabled" {
  type        = bool
  description = "Template the Vault field GIT_SSH_KEY to a key file and point GIT_SSH_COMMAND at it, so `sync` can clone git@ URLs. Use a read-only deploy key."
  default     = false
}

variable "git_known_hosts" {
  type        = string
  description = "known_hosts content used when git_ssh_key_enabled (strict host checking). Default is GitHub's ed25519 host key as published at https://api.github.com/meta (ssh_keys)."
  default     = "github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
}

# --- Resources ---

variable "task_resources_cpu" {
  type        = number
  description = "CPU (MHz) for the app task. plan runs up to DEPLOY_PLAN_PARALLELISM (default 6) terraform processes at once."
  default     = 1000
}

variable "task_resources_memory" {
  type        = number
  description = "Memory (MB) for the app task. A starting guess, not measured: watch a real plan and adjust."
  default     = 2048
}
