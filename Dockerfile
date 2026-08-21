# reach in a container.
#
# reach normally runs on the operator's own machine, next to their ssh config
# and their agent. This image is for the case where that machine is itself
# disposable — a CI job, a devcontainer, a jump box — and installing a binary is
# more ceremony than pulling one.
#
# Two things have to be here that a scratch image would not give:
#
#   openssh-client, because the SSH transport shells out to `ssh` for
#   ControlMaster multiplexing rather than reimplementing it. Without the
#   binary, every transport in reach fails at the first command.
#
#   ca-certificates, because a target reached through a bastion or a proxy that
#   terminates TLS needs a trust store, and an empty one fails in a way that
#   reads like the host is down.
#
# The helper binaries sit beside reach on purpose: LocateHelperBinary looks in
# the directory holding the running executable first, so the helper tier works
# in this image without a Go toolchain and without downloading anything onto
# somebody else's server at run time.
FROM alpine:3.21

RUN apk add --no-cache openssh-client ca-certificates

COPY reach /usr/local/bin/reach
COPY .helpers/ /usr/local/bin/

# An ssh key and a known_hosts file have to come from the host; there is nothing
# useful to bake in. Mount them read-only:
#   docker run --rm -v "$HOME/.ssh:/root/.ssh:ro" ghcr.io/bojieli/agentreach ...
ENTRYPOINT ["/usr/local/bin/reach"]
