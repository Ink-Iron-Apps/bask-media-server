# Bask Media Server in a container.
#
# A general-purpose image: nothing in it is specific to any one installation.
# No paths, no libraries, no keys. It runs with no arguments at all, and it
# takes a mounted configuration file when there is one, which is how anybody
# running it for real will want to drive it.
#
# The build is static because the SQLite driver is pure Go, so the runtime
# image needs no libc and carries ffmpeg only -- which is there for converting
# files a client cannot play and for finding intro and credits markers. Direct
# play needs neither, so a smaller image without it would still work for most
# of what this does; it is included because being told to install something
# after the fact is exactly what this project is trying to avoid.

# On the runner's own architecture, whatever is being built for: compiling Go
# for another one is a cross-compile, and emulating that architecture to run a
# native compiler in it costs tens of minutes for nothing.
#
# BUILDPLATFORM is predefined by buildx and overrides this default. The default
# is here so that a plain `docker build` on the older builder, which defines no
# such thing, does not fail on an empty --platform.
ARG BUILDPLATFORM=linux/amd64
FROM --platform=${BUILDPLATFORM} golang:1.27-alpine AS build
WORKDIR /src

# Dependencies first, so editing the source does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=0.0.0-container
ARG TARGETARCH
RUN CGO_ENABLED=0 GOARCH=${TARGETARCH:-amd64} go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/bms ./cmd/bms

FROM alpine:3.22
RUN apk add --no-cache ffmpeg ca-certificates tzdata

COPY --from=build /out/bms /usr/local/bin/bms
COPY packaging/bms.conf.example /usr/share/bask-media-server/bms.conf.example
COPY packaging/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

# 1000 on purpose, not whatever the distribution hands out next. It is the
# first user on almost every desktop and NAS, so media owned by the person who
# downloaded it is usually readable with no --user and no chown. Where it is
# not, --user is the answer and the README says so.
RUN addgroup -S -g 1000 bms \
 && adduser -S -u 1000 -G bms -H -D bms \
 && mkdir -p /var/lib/bask-media-server /config \
 && chown bms:bms /var/lib/bask-media-server /config

# The database, artwork cache and backups live here, and the configuration file
# goes in /config. Mount a volume over the first or everything is lost with the
# container.
VOLUME /var/lib/bask-media-server
EXPOSE 32500

# /identity is unauthenticated and answers from the library database, so it
# tells the truth about whether this is actually serving. It uses the default
# address; a configuration file that moves the port wants --health-cmd with it.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD wget -q -O- http://127.0.0.1:32500/identity >/dev/null || exit 1

USER bms
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
