# The pestilence control plane: a static Go binary and nothing else.
#
# This is the trusted plane. It holds the platform's Kubernetes credential -- which
# is read/write on Secrets in every namespace -- and it mints each workspace's token
# signing key. So the image is kept minimal deliberately: distroless (no shell, no
# package manager), non-root, no capabilities.
#
# The pestilence namespace enforces Pod Security Admission `restricted`, the same
# profile as tenant namespaces, so the image must run as a non-root user and need no
# capabilities. The PodSpec sets RuntimeDefault seccomp and drops ALL.
#
# It is never exposed publicly. town is its own deployment and reaches this over the
# cluster network, which is also why pestilence needs no CORS and no Ingress.

FROM gcr.io/distroless/static-debian12:nonroot

# Cross-compiled by hack/load-image.sh into this build context.
#
# CGO_ENABLED=0 matters: the SQLite driver is pure Go (modernc.org/sqlite) precisely
# because there is no C toolchain in this image and no libc to link against.
COPY pestilence-control-plane /pestilence-control-plane

# Numeric, not `nonroot`: with runAsNonRoot the kubelet must be able to verify the
# user is non-root, and it cannot resolve a named user from the image config.
# 65532 is distroless's nonroot uid.
USER 65532:65532

ENTRYPOINT ["/pestilence-control-plane"]
