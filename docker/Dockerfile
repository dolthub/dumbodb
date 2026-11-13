# syntax=docker/dockerfile:1.3-labs

# Ubuntu 24.04 carries libicu74. The release workflow builds release binaries
# on ubuntu-24.04 / ubuntu-24.04-arm runners, so this base ships the exact
# ICU runtime the downloaded binaries are linked against. Aligning here
# avoids the "libicui18n.so.74: cannot open shared object file" runtime
# failure that older debian:bookworm-slim (ICU 72) produced.
#
# The runtime image builds on this stage, so it installs only what the server
# needs: git for git remotes (Dolt's git blobstore runs the git command), tini,
# and CA certificates for https remotes. curl is installed only in the
# download stage.
FROM ubuntu:24.04 AS base
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -y && \
    apt-get install -y --no-install-recommends \
        git tini ca-certificates && \
    rm -rf /var/lib/apt/lists/*


# Source build runs on the same ubuntu:24.04 base so the binary it produces
# links against the same ICU as the runtime image, and so the source-build
# and download-binary paths are interchangeable.
FROM ubuntu:24.04 AS build-from-source
ENV DEBIAN_FRONTEND=noninteractive
ARG DUMBODB_VERSION
# TARGETARCH is provided automatically by BuildKit/buildx (amd64, arm64, ...).
ARG TARGETARCH
ARG GO_VERSION=1.27.3

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
RUN apt-get update -y && \
    apt-get install -y --no-install-recommends curl && \
    rm -rf /var/lib/apt/lists/*

# Downloads the release binary and installs it only if it matches the
# release's SHA256SUMS. Inline so the same Dockerfile works for release and
# source builds, whose build contexts differ.
COPY <<'EOF' /usr/local/bin/fetch-dumbodb.sh
#!/bin/sh
set -eu

if [ "$DUMBODB_VERSION" = "source" ]; then
	exit 0
fi

if [ "$DUMBODB_VERSION" = "latest" ]; then
	DUMBODB_VERSION=$(curl -fsSL https://api.github.com/repos/dolthub/dumbodb/releases/latest |
		grep '"tag_name"' | cut -d'"' -f4 | sed 's/^v//')
	if [ -z "$DUMBODB_VERSION" ]; then
		echo "could not resolve the latest DumboDB release" >&2
		exit 1
	fi
fi

release="https://github.com/dolthub/dumbodb/releases/download/v${DUMBODB_VERSION}"
asset="dumbodb-linux-${TARGETARCH}"
work=$(mktemp -d)
cd "$work"

curl -fsSL -o "$asset" "$release/$asset"
curl -fsSL -o SHA256SUMS "$release/SHA256SUMS"
grep " ${asset}\$" SHA256SUMS > expected || {
	echo "SHA256SUMS has no entry for $asset" >&2
	exit 1
}
sha256sum -c expected

install -m 0755 "$asset" /usr/local/bin/dumbodb
EOF
RUN chmod 755 /usr/local/bin/fetch-dumbodb.sh && /usr/local/bin/fetch-dumbodb.sh


# Fetches MongoDB's apt signing key for the mongosh package and keeps only
# the key with the pinned fingerprint, as the official mongo image does. Only
# the keyring file reaches the runtime image, not curl or gnupg.
FROM base AS mongodb-keyring
ARG MONGODB_KEY_URL=https://pgp.mongodb.com/server-8.0.asc
ARG MONGODB_KEY_FINGERPRINT=4B0752C1BCA238C0B4EE14DC41DE058A4E7DCA05
ARG MONGODB_KEYRING=/etc/apt/keyrings/mongodb.asc
RUN apt-get update -y && \
    apt-get install -y --no-install-recommends curl gnupg && \
    rm -rf /var/lib/apt/lists/*

COPY <<'EOF' /usr/local/bin/fetch-mongodb-keyring.sh
#!/bin/sh
set -eu

GNUPGHOME=$(mktemp -d)
export GNUPGHOME

curl -fsSL -o "$GNUPGHOME/KEYS" "$MONGODB_KEY_URL"
gpg --batch --quiet --import "$GNUPGHOME/KEYS"
mkdir -p "$(dirname "$MONGODB_KEYRING")"
gpg --batch --export --armor "$MONGODB_KEY_FINGERPRINT" > "$MONGODB_KEYRING"
rm -rf "$GNUPGHOME"

if [ ! -s "$MONGODB_KEYRING" ]; then
	echo "$MONGODB_KEY_URL has no key with fingerprint $MONGODB_KEY_FINGERPRINT" >&2
	exit 1
fi
EOF
RUN chmod 755 /usr/local/bin/fetch-mongodb-keyring.sh && /usr/local/bin/fetch-mongodb-keyring.sh


FROM base AS runtime
ARG DUMBODB_VERSION
# mongosh, as in the official mongo image: used through docker exec (including
# the --auth localhost-exception bootstrap), to create the root user and to run
# /docker-entrypoint-initdb.d scripts.
ARG MONGOSH_VERSION=2.13.0

COPY --from=mongodb-keyring /etc/apt/keyrings/mongodb.asc /etc/apt/keyrings/mongodb.asc

# dumbodb is dynamically linked against ICU (libicui18n / libicuuc /
# libicudata) via the transitive go-icu-regex CGo binding. Install the
# runtime-only ICU package unconditionally -- the download-binary path
# needs it just as much as the source-build path. libicu74 is the
# matching version on ubuntu:24.04. apt verifies mongosh against the pinned
# MongoDB key.
RUN echo "deb [ signed-by=/etc/apt/keyrings/mongodb.asc ] https://repo.mongodb.org/apt/ubuntu noble/mongodb-org/8.0 multiverse" \
        > /etc/apt/sources.list.d/mongodb-org.list && \
    apt-get update -y && \
    apt-get install -y --no-install-recommends libicu74 "mongodb-mongosh=${MONGOSH_VERSION}" && \
    rm -rf /var/lib/apt/lists/* && \
    mongosh --version

# Only one binary is possible due to DUMBODB_VERSION, so we optionally copy from either stage
COPY --from=download-binary /usr/local/bin/dumbodb* /usr/local/bin/
COPY --from=build-from-source /usr/local/bin/dumbodb* /usr/local/bin/

RUN /usr/local/bin/dumbodb --version

# The server runs as the unprivileged dumbodb user (uid/gid 999), as mongod
# does in the official MongoDB image. The image has no USER directive: the
# entrypoint starts as root only to hand /var/lib/dumbodb to dumbodb, which a
# root-owned bind mount needs, then drops privileges before starting the
# server. A container started with --user skips that step.
RUN groupadd --gid 999 --system dumbodb && \
    useradd --uid 999 --system --gid dumbodb --home-dir /var/lib/dumbodb dumbodb && \
    mkdir -p /var/lib/dumbodb /docker-entrypoint-initdb.d && \
    chown dumbodb:dumbodb /var/lib/dumbodb && \
    chmod 700 /var/lib/dumbodb

# Inline rather than COPYed from the build context, whose layout differs
# between release and source builds.
COPY <<'EOF' /usr/local/bin/docker-entrypoint.sh
#!/bin/bash
# Modeled on the official mongo image's entrypoint (docker-library/mongo).
set -Eeuo pipefail

# Flags alone ("docker run dumbodb --auth") are flags for the server.
if [ "$#" -gt 0 ] && [ "${1#-}" != "$1" ]; then
	set -- dumbodb "$@"
fi

# has_flag NAME ARGS... reports whether ARGS set the Go flag NAME, in any of
# the forms the flag package accepts (-name, --name, -name=v, --name=v).
has_flag() {
	local name=$1 arg
	shift
	for arg; do
		case "$arg" in
		-"$name" | --"$name" | -"$name"=* | --"$name"=*) return 0 ;;
		esac
	done
	return 1
}

# flag_value NAME ARGS... prints the value ARGS give the Go flag NAME.
flag_value() {
	local name=$1 value='' next='' arg
	shift
	for arg; do
		if [ -n "$next" ]; then
			value=$arg
			next=''
			continue
		fi
		case "$arg" in
		-"$name" | --"$name") next=1 ;;
		-"$name"=* | --"$name"=*) value=${arg#*=} ;;
		esac
	done
	printf '%s' "$value"
}

# As the mongo image adds --bind_ip_all, listen on every interface and use the
# image's volume unless the command line says otherwise; a lone --port still
# sets the port.
if [ "${1:-}" = 'dumbodb' ]; then
	has_flag addr "$@" || set -- "$@" --addr 0.0.0.0:27017
	has_flag data-dir "$@" || set -- "$@" --data-dir /var/lib/dumbodb
fi

if [ "${1:-}" = 'dumbodb' ] && [ "$(id -u)" = '0' ]; then
	find -L /var/lib/dumbodb \! -user dumbodb -exec chown dumbodb:dumbodb '{}' +
	# Lets the server keep writing logs to the container's stdout and stderr.
	chown --dereference dumbodb "/proc/$$/fd/1" "/proc/$$/fd/2" || :
	exec setpriv --reuid=dumbodb --regid=dumbodb --init-groups "$0" "$@"
fi

# file_env VAR prints VAR, or the contents of the file VAR_FILE names; setting
# both is an error.
file_env() {
	local var=$1 file_var="${1}_FILE"
	local value=${!var:-} file=${!file_var:-}
	if [ -n "$value" ] && [ -n "$file" ]; then
		echo >&2 "error: both $var and $file_var are set (but are exclusive)"
		return 1
	fi
	if [ -n "$file" ]; then
		if [ ! -r "$file" ]; then
			echo >&2 "error: $file_var names $file, which cannot be read"
			return 1
		fi
		value=$(< "$file")
	fi
	printf '%s' "$value"
}

# init_var NAME prints MONGO_INITDB_NAME or its synonym DUMBO_INITDB_NAME
# (either possibly as _FILE); both may be set only to the same value.
init_var() {
	local mongo dumbo
	mongo=$(file_env "MONGO_INITDB_$1") || return 1
	dumbo=$(file_env "DUMBO_INITDB_$1") || return 1
	if [ -n "$mongo" ] && [ -n "$dumbo" ] && [ "$mongo" != "$dumbo" ]; then
		echo >&2 "error: MONGO_INITDB_$1 and DUMBO_INITDB_$1 are set to different values"
		return 1
	fi
	printf '%s' "${mongo:-$dumbo}"
}

# is_fresh DIR reports whether the data directory holds no databases yet;
# DumboDB's own dot-prefixed directories do not count.
is_fresh() {
	local entry
	for entry in "$1"/*/; do
		[ -d "$entry" ] && return 1
	done
	return 0
}

# has_init_scripts reports whether /docker-entrypoint-initdb.d holds scripts
# to run; only *.sh and *.js files count.
has_init_scripts() {
	local f
	for f in /docker-entrypoint-initdb.d/*; do
		case "$f" in
		*.sh | *.js) return 0 ;;
		esac
	done
	return 1
}

# initialize ARGS... starts a temporary server on 127.0.0.1 with the server's
# arguments, minus authentication, replication and listen settings, creates
# the root user if one is configured, runs the init scripts and stops it.
initialize() {
	local temp_args=() skip='' tls_cert='' arg pid tries
	shift
	for arg; do
		if [ -n "$skip" ]; then
			skip=''
			continue
		fi
		case "$arg" in
		-auth | --auth | -auth=* | --auth=*) continue ;;
		-addr | --addr | -port | --port | -replSet | --replSet | -keyFile | --keyFile | \
			-clusterAuthMode | --clusterAuthMode | -tlsMode | --tlsMode)
			skip=1
			continue
			;;
		-addr=* | --addr=* | -port=* | --port=* | -replSet=* | --replSet=* | -keyFile=* | --keyFile=* | \
			-clusterAuthMode=* | --clusterAuthMode=* | -tlsMode=* | --tlsMode=*)
			continue
			;;
		-tlsCertificateKeyFile | --tlsCertificateKeyFile | -tlsCertificateKeyFile=* | --tlsCertificateKeyFile=*)
			tls_cert=1
			;;
		esac
		temp_args+=("$arg")
	done
	# As in the mongo image: TLS stays available if configured, but plaintext
	# local connections must work.
	if [ -n "$tls_cert" ]; then
		temp_args+=(--tlsMode allowTLS)
	fi
	temp_args+=(--addr 127.0.0.1:27017)

	dumbodb "${temp_args[@]}" &
	pid=$!

	local mongo=(mongosh --host 127.0.0.1 --port 27017 --quiet)
	for tries in {30..0}; do
		if "${mongo[@]}" admin --eval 'quit(db.adminCommand({ping: 1}).ok ? 0 : 2)' > /dev/null 2>&1; then
			break
		fi
		if ! kill -0 "$pid" 2> /dev/null; then
			echo >&2 "error: the temporary server for initialization exited"
			exit 1
		fi
		sleep 1
	done
	if [ "$tries" = 0 ]; then
		echo >&2 "error: the temporary server for initialization did not start"
		kill "$pid" || :
		exit 1
	fi

	if [ -n "$root_user" ]; then
		# The credentials reach mongosh through its environment, never as code.
		INITDB_ROOT_USERNAME=$root_user INITDB_ROOT_PASSWORD=$root_password "${mongo[@]}" admin --eval '
			db.createUser({
				user: process.env.INITDB_ROOT_USERNAME,
				pwd: process.env.INITDB_ROOT_PASSWORD,
				roles: [{role: "root", db: "admin"}],
			})' > /dev/null
	fi

	# As in the mongo image: *.sh scripts are sourced (and can use the mongo
	# array), *.js scripts run against MONGO_INITDB_DATABASE.
	export MONGO_INITDB_DATABASE=$init_database
	local f
	for f in /docker-entrypoint-initdb.d/*; do
		case "$f" in
		*.sh)
			echo >&2 "$0: running $f"
			# shellcheck disable=SC1090
			. "$f"
			;;
		*.js)
			echo >&2 "$0: running $f"
			"${mongo[@]}" "$MONGO_INITDB_DATABASE" "$f"
			;;
		*)
			[ -e "$f" ] && echo >&2 "$0: ignoring $f"
			;;
		esac
	done

	kill -TERM "$pid"
	wait "$pid" || :
}

if [ "${1:-}" = 'dumbodb' ]; then
	root_user=$(init_var ROOT_USERNAME)
	root_password=$(init_var ROOT_PASSWORD)
	init_database=$(init_var DATABASE)
	init_database=${init_database:-test}
	if [ -n "$root_user" ] || [ -n "$root_password" ]; then
		if [ -z "$root_user" ] || [ -z "$root_password" ]; then
			echo >&2 "error: missing 'MONGO_INITDB_ROOT_USERNAME' or 'MONGO_INITDB_ROOT_PASSWORD'"
			echo >&2 "       (or DUMBO_INITDB_ROOT_USERNAME / DUMBO_INITDB_ROOT_PASSWORD);"
			echo >&2 "       both must be specified for a user to be created"
			exit 1
		fi
		has_flag auth "$@" || set -- "$@" --auth
	fi
	if { [ -n "$root_user" ] || has_init_scripts; } && is_fresh "$(flag_value data-dir "$@")"; then
		initialize "$@"
	fi
	unset "${!MONGO_INITDB_@}" "${!DUMBO_INITDB_@}"
fi

exec "$@"
EOF
RUN chmod 755 /usr/local/bin/docker-entrypoint.sh

VOLUME /var/lib/dumbodb
EXPOSE 27017
WORKDIR /var/lib/dumbodb
ENTRYPOINT ["tini", "--", "/usr/local/bin/docker-entrypoint.sh"]
CMD ["dumbodb"]
