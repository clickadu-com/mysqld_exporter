ARG GolangVersion=1.24.0-202502180835
ARG ARCH="amd64"
ARG OS="linux"

FROM nexus.adsrv.wtf/click/golang:${GolangVersion} as build
COPY --chown=jenkins:jenkins . ./
ARG BUILD_VERSION=""
ARG GOAMD64=v3
ARG ARCH="amd64"
ARG OS="linux"
RUN --mount=type=cache,target=${GOPATH},mode=0777,uid=10000,gid=10000 make build


FROM quay.io/prometheus/busybox-${OS}-${ARCH}:latest
LABEL maintainer="The Prometheus Authors <prometheus-developers@googlegroups.com>"
ARG ARCH="amd64"
ARG OS="linux"
COPY --from=build /app/mysqld_exporter /bin/mysqld_exporter

EXPOSE      9104
USER        nobody
ENTRYPOINT  [ "/bin/mysqld_exporter" ]
