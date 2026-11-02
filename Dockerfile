# homelab-deploy: the CLI and its `serve` web dashboard.
#
# What the running container needs besides the binary:
#   - terraform  every plan/apply/init shells out to it (latest release at
#                build time, copied from HashiCorp's official image)
#   - git, ssh   `sync` clones and updates the repos in .deploy-repos
#   - CA certs   HTTPS to providers and registries
#
# The working directory is /work: mount the repo to operate on there (or
# let `sync` populate it) and put secrets in via environment or mounted
# files at run time. Nothing from the build context's .env files is ever
# copied in (see .dockerignore).

FROM hashicorp/terraform:latest AS terraform

FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Same flags as the release workflow's binary.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/homelab-deploy .

FROM alpine:3
RUN apk add --no-cache git openssh-client ca-certificates \
 && adduser -D -u 10001 -h /home/deploy deploy \
 && mkdir /work \
 && chown deploy /work
COPY --from=terraform /bin/terraform /usr/local/bin/terraform
COPY --from=build /out/homelab-deploy /usr/local/bin/homelab-deploy

USER deploy
WORKDIR /work
EXPOSE 8080
ENTRYPOINT ["homelab-deploy"]
CMD ["serve", ":8080"]
