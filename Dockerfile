# Base images are pinned by digest in addition to the tag so a re-tag upstream
# can't silently change what we build/ship. To refresh after a deliberate base
# bump, resolve the new multi-arch index digest with:
#   docker buildx imagetools inspect golang:1.24-alpine3.20   # copy the Digest
#   docker buildx imagetools inspect alpine:3.18
# The build stage runs natively on the BUILD platform (no QEMU) and
# cross-compiles to the TARGET platform with Go's own toolchain. The agent is
# now CGO-free (pure-Go GORM + Postgres + glebarez SQLite drivers), so no C
# toolchain or sqlite-dev is needed and the resulting binary is already static.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine3.20@sha256:9f98e9893fbc798c710f3432baa1e0ac6127799127c3101d2c263c3a954f0abe AS build_deps

RUN apk add --no-cache git

WORKDIR /workspace

COPY go.mod .
COPY go.sum .

RUN go mod download

FROM build_deps AS build

COPY . .

ARG TARGETOS TARGETARCH
# VERSION is injected by the release pipeline (the pushed git tag) and stamped
# into version.Version via -ldflags -X. Defaults to "dev" for plain builds.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o runos \
    -trimpath \
    -ldflags "-w -X github.com/runos-official/clusteragent/version.Version=${VERSION}" .

FROM alpine:3.18@sha256:de0eb0b3f2a47ba1eb89389859a9bd88b28e82f5826b6969ad604979713c2d4f

ARG TARGETARCH

# Supply-chain pins. kubectl and buildctl are downloaded over the network at
# build time, so both are pinned to an exact version and verified against a
# known-good sha256 (per arch); a mismatch fails the build (sha256sum -c).
#
# To refresh kubectl: pick a version, then for each arch fetch the official
# checksum: curl -L "https://dl.k8s.io/release/<ver>/bin/linux/<arch>/kubectl.sha256"
# To refresh buildkit: download the release tarball for each arch and
# `sha256sum` it (moby/buildkit publishes no per-asset .sha256 sidecar):
#   curl -L .../buildkit-<ver>.linux-<arch>.tar.gz | sha256sum
ENV KUBECTL_VERSION=v1.36.2
ENV BUILDKIT_VERSION=v0.17.3
# kubectl checksums (linux/{amd64,arm64}) for KUBECTL_VERSION above.
ENV KUBECTL_SHA256_amd64=1e9045ec32bea85da43de85f0065358529ea7c7a152eca78154fba5b58c27d82
ENV KUBECTL_SHA256_arm64=c957eb8c4bea27a3bb35b269edd9082e27f027f7b76b20b5bf4afebc726c6d3e
# buildkit tarball checksums (linux/{amd64,arm64}) for BUILDKIT_VERSION above.
ENV BUILDKIT_SHA256_amd64=1ab54cc01fd2e174483070451921badc53cc463a4f2e2e980be7db99ca95c0d0
ENV BUILDKIT_SHA256_arm64=1afabc9c0829f7fa5173f439b7212194703b48f8e79a405596ada1af6e6f8220

RUN apk add --no-cache ca-certificates wget curl git && \
    ARCH=${TARGETARCH:-amd64} && \
    case "$ARCH" in \
      amd64) BUILDKIT_SHA="$BUILDKIT_SHA256_amd64"; KUBECTL_SHA="$KUBECTL_SHA256_amd64" ;; \
      arm64) BUILDKIT_SHA="$BUILDKIT_SHA256_arm64"; KUBECTL_SHA="$KUBECTL_SHA256_arm64" ;; \
      *) echo "unsupported TARGETARCH: $ARCH" >&2; exit 1 ;; \
    esac && \
    wget -O /tmp/buildkit.tar.gz "https://github.com/moby/buildkit/releases/download/${BUILDKIT_VERSION}/buildkit-${BUILDKIT_VERSION}.linux-${ARCH}.tar.gz" && \
    echo "${BUILDKIT_SHA}  /tmp/buildkit.tar.gz" | sha256sum -c - && \
    tar -xzf /tmp/buildkit.tar.gz -C /usr/local bin/buildctl && \
    rm /tmp/buildkit.tar.gz && \
    curl -fsSLO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${ARCH}/kubectl" && \
    echo "${KUBECTL_SHA}  kubectl" | sha256sum -c - && \
    chmod +x kubectl && \
    mv kubectl /usr/local/bin/kubectl

COPY --from=build /workspace/runos /usr/local/bin/runos

# Runs as root deliberately (no non-root USER). The agent shells out to
# kubectl/helm/buildctl for privileged in-cluster operations; dropping to a
# non-root UID here breaks the tooling without a coordinated permission change.
# State now lives in Postgres (no local /data PVC), so persistence no longer
# constrains the UID. The deployment manifest documents the same rationale at
# the pod securityContext. Revisit together if hardening to non-root.
ENTRYPOINT ["runos"]
