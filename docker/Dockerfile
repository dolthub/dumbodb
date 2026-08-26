# syntax=docker/dockerfile:1.3-labs

# Ubuntu 24.04 carries libicu74. The release workflow builds release binaries
# on ubuntu-24.04 / ubuntu-24.04-arm runners, so this base ships the exact
# ICU runtime the downloaded binaries are linked against. Aligning here
# avoids the "libicui18n.so.74: cannot open shared object file" runtime
# failure that older debian:bookworm-slim (ICU 72) produced.
FROM ubuntu:24.04 AS base
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -y && \
    apt-get install -y --no-install-recommends \
        curl git tini ca-certificates && \
    rm -rf /var/lib/apt/lists/*


# Source build runs on the same ubuntu:24.04 base so the binary it produces
# links against the same ICU as the runtime image, and so the source-build
# and download-binary paths are interchangeable.
FROM ubuntu:24.04 AS build-from-source
ENV DEBIAN_FRONTEND=noninteractive
ARG DUMBODB_VERSION
# TARGETARCH is provided automatically by BuildKit/buildx (amd64, arm64, ...).
ARG TARGETARCH
ARG GO_VERSION=1.26.7

# COPY doesn't support conditionals, so we rely on the build context maybe having a dumbodb/ directory
# to distinguish between source and binary builds using DUMBODB_VERSION=source.
COPY dumbodb*/go.mod* dumbodb*/go.sum* /tmp/dumbodb/
WORKDIR /tmp/dumbodb/

# Check for source to avoid unnecessary installation of build dependencies.
# Go is not in ubuntu 24.04's archive at 1.26+; install from the official
# tarball.
RUN if [ "$DUMBODB_VERSION" = "source" ]; then \
        cd /tmp/dumbodb || { echo "Make sure the dumbodb/ directory exists in your workspace to build from source."; exit 1; }; \
        apt-get update -y && \
        apt-get install -y --no-install-recommends ca-certificates curl libicu-dev gcc g++ && \
        curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" \
            | tar -xz -C /usr/local && \
        rm -rf /var/lib/apt/lists/*; \
    fi
ENV PATH=/usr/local/go/bin:$PATH

# Separate layers to avoid redundant downloads
RUN if [ "$DUMBODB_VERSION" = "source" ]; then \
        go mod download; \
    fi

COPY dumbodb*/ /tmp/dumbodb/

RUN if [ "$DUMBODB_VERSION" = "source" ]; then \
        go build -o /usr/local/bin/dumbodb ./cmd/dumbodb && \
        chmod +x /usr/local/bin/dumbodb; \
    fi


FROM base AS download-binary
ARG DUMBODB_VERSION
# TARGETARCH is provided automatically by BuildKit/buildx (amd64, arm64, ...).
# It matches the suffix on release artifacts published by .github/workflows/release.yml.
ARG TARGETARCH
RUN if [ "$DUMBODB_VERSION" = "latest" ]; then \
        # Fetch latest version number from GitHub API
        DUMBODB_VERSION=$(curl -s https://api.github.com/repos/dolthub/dumbodb/releases/latest \
            | grep '"tag_name"' \
            | cut -d'"' -f4 \
            | sed 's/^v//'); \
    fi && \
    if [ "$DUMBODB_VERSION" != "source" ]; then \
        curl -fL -o /usr/local/bin/dumbodb \
            "https://github.com/dolthub/dumbodb/releases/download/v${DUMBODB_VERSION}/dumbodb-linux-${TARGETARCH}" && \
        chmod +x /usr/local/bin/dumbodb; \
    fi


FROM base AS runtime
ARG DUMBODB_VERSION

# dumbodb is dynamically linked against ICU (libicui18n / libicuuc /
# libicudata) via the transitive go-icu-regex CGo binding. Install the
# runtime-only ICU package unconditionally -- the download-binary path
# needs it just as much as the source-build path. libicu74 is the
# matching version on ubuntu:24.04.
RUN apt-get update -y && \
    apt-get install -y --no-install-recommends libicu74 && \
    rm -rf /var/lib/apt/lists/*

# Only one binary is possible due to DUMBODB_VERSION, so we optionally copy from either stage
COPY --from=download-binary /usr/local/bin/dumbodb* /usr/local/bin/
COPY --from=build-from-source /usr/local/bin/dumbodb* /usr/local/bin/

RUN /usr/local/bin/dumbodb --version

RUN mkdir -p /var/lib/dumbodb && chmod 755 /var/lib/dumbodb

VOLUME /var/lib/dumbodb
EXPOSE 27017
WORKDIR /var/lib/dumbodb
ENTRYPOINT ["tini", "--", "/usr/local/bin/dumbodb"]
CMD ["--data-dir", "/var/lib/dumbodb", "--addr", "0.0.0.0:27017"]
