# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# SQLite-драйвер на чистом Go, поэтому CGO не нужен и бинарник статический.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/financer . \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/financer /app/financer
# Каталог данных принадлежит пользователю nonroot (65532), иначе том будет недоступен для записи.
COPY --from=build --chown=65532:65532 /out/data /data
ENV DATA_FILE=/data/financer.db
VOLUME ["/data"]
USER nonroot:nonroot
ENTRYPOINT ["/app/financer"]
