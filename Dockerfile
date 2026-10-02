# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/shugo ./cmd/shugo \
 && mkdir /out/data

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/shugo /shugo
# A named volume mounted on /data starts out with this directory's owner, so
# the nonroot user (65532) can create the database in it.
COPY --from=build --chown=65532:65532 /out/data /data
ENV DATABASE_PATH=/data/shugo.db
USER nonroot:nonroot
ENTRYPOINT ["/shugo"]
