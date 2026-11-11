# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
ENV GOOS=$TARGETOS GOARCH=$TARGETARCH
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/wickfleet-hub ./cmd/wickfleet-hub \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/wickfleet-dockerguard ./cmd/wickfleet-dockerguard

# Alpine, not distroless: registry writes shell out to git over ssh.
FROM alpine:3.22.2@sha256:4b7ce07002c69e8f3d704a9c5d6fd3053be500b7f1c69fc0d80990c2ad8dd412
RUN apk add --no-cache git openssh-client ca-certificates \
 && adduser -D -H -u 65532 -s /sbin/nologin nonroot
COPY --from=build /out/wickfleet-hub /wickfleet-hub
# Same image, second entrypoint: the guard runs as its own container.
COPY --from=build /out/wickfleet-dockerguard /wickfleet-dockerguard
USER 65532
EXPOSE 8090
ENTRYPOINT ["/wickfleet-hub"]
