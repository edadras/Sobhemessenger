# Built from the repository root so the image can carry database/migrations
# alongside the binaries:  docker build -f backend/Dockerfile .
FROM golang:1.24-alpine AS build

WORKDIR /src

# Dependencies are copied first so the module cache survives source edits.
COPY backend/go.mod backend/go.sum ./
RUN go mod download

COPY backend/ ./

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/api ./cmd/api \
 && CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags="-s -w" \
        -o /out/migrate ./cmd/migrate \
 && CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags="-s -w" \
        -o /out/worker ./cmd/worker

# Runtime stage: no shell, no package manager, non-root (§33).
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/api /usr/local/bin/api
COPY --from=build /out/migrate /usr/local/bin/migrate
COPY --from=build /out/worker /usr/local/bin/worker
COPY database/migrations /app/migrations
COPY database/seeds /app/seeds

USER nonroot:nonroot
WORKDIR /app

EXPOSE 8080 9090
ENV MIGRATIONS_DIR=/app/migrations

ENTRYPOINT ["/usr/local/bin/api"]
