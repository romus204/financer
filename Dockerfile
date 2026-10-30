# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The SQLite driver is pure Go, so CGO is not needed and the binary is static.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/financer . \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/financer /app/financer
# The data directory is owned by the nonroot user (65532); otherwise the volume would not be writable.
COPY --from=build --chown=65532:65532 /out/data /data
ENV DATA_FILE=/data/financer.db
VOLUME ["/data"]
USER nonroot:nonroot
ENTRYPOINT ["/app/financer"]
