# cf-proxy harness image — nginx + the harness's tenant-routing config baked
# in at build time.
#
# Why bake (not bind-mount): on Gitea Actions / act_runner, the runner is a
# container talking to the OUTER docker daemon over the host socket; runc
# resolves bind-mount source paths on the outer host filesystem, where the
# repo at `/workspace/.../tests/harness/cf-proxy/nginx.conf` is invisible.
# Compose `configs:` (with `file:`) falls back to bind mounts when swarm is
# not active, so it hits the same gap. A build-time COPY uploads the file
# as part of the docker build context — the daemon receives the tarball
# directly and never bind-mounts. See issue #88 item 2.
FROM nginx:1.27-alpine

COPY nginx.conf /etc/nginx/nginx.conf
