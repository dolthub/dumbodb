# syntax=docker/dockerfile:1.3-labs

FROM debian:bookworm-slim AS base
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -y && \
    apt-get install -y --no-install-recommends \
        curl git tini ca-certificates && \
    rm -rf /var/lib/apt/lists/*


# We use bookworm since the icu dependency ver. between the base and golang images is the same
FROM golang:1.26.2-bookworm AS build-from-source
ENV DEBIAN_FRONTEND=noninteractive
ARG DUMBODB_VERSION

# COPY doesn't support conditionals, so we rely on the build context maybe having a dumbodb/ directory
# to distinguish between source and binary builds using DUMBODB_VERSION=source.
COPY dumbodb*/go.mod* dumbodb*/go.sum* /tmp/dumbodb/
WORKDIR /tmp/dumbodb/

# Check for source to avoid unnecessary installation of build dependencies
RUN if [ "$DUMBODB_VERSION" = "source" ]; then \
        cd /tmp/dumbodb || { echo "Make sure the dumbodb/ directory exists in your workspace to build from source."; exit 1; }; \
        apt-get update -y && \
        apt-get install -y libicu-dev && \
        rm -rf /var/lib/apt/lists/*; \
    fi

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
RUN if [ "$DUMBODB_VERSION" = "latest" ]; then \
        # Fetch latest version number from GitHub API
        DUMBODB_VERSION=$(curl -s https://api.github.com/repos/dolthub/dumbodb/releases/latest \
            | grep '"tag_name"' \
            | cut -d'"' -f4 \
            | sed 's/^v//'); \
    fi && \
    if [ "$DUMBODB_VERSION" != "source" ]; then \
        curl -L "https://github.com/dolthub/dumbodb/releases/download/v${DUMBODB_VERSION}/install.sh" | bash; \
    fi


FROM base AS runtime
ARG DUMBODB_VERSION

# icu dependency for source builds (transitively required by dolt/vitess)
RUN if [ "$DUMBODB_VERSION" = "source" ]; then \
        apt-get update -y && \
        apt-get install -y --no-install-recommends libicu-dev && \
        rm -rf /var/lib/apt/lists/*; \
    fi

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
