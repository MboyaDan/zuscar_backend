# syntax=docker/dockerfile:1
# UNVERIFIED in the authoring sandbox (no Docker here). The last stage is the production image, so a plain
# `docker build .` (what Render does) produces `runtime`. Docker Compose builds the `dev` target.
ARG GO_VERSION=1.27

FROM golang:${GO_VERSION} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
 && go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate \
 && go build -trimpath -ldflags="-s -w" -o /out/bootstrap ./cmd/bootstrap

# Tests (`docker compose run --rm test`). Needs TEST_DATABASE_URL pointing at a server where it may create databases.
FROM build AS test
CMD ["go", "test", "-count=1", "./..."]

# Fictional seed data exists only in the dev image, never in the production image.
FROM build AS seed-build
RUN go build -trimpath -ldflags="-s -w" -o /out/seed-dev ./cmd/seed-dev

FROM gcr.io/distroless/static-debian12:nonroot AS dev
WORKDIR /app
COPY --from=build /out/api /out/migrate /out/bootstrap /app/
COPY --from=seed-build /out/seed-dev /app/seed-dev
COPY database/migrations /app/database/migrations
ENV MIGRATIONS_DIR=/app/database/migrations
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s CMD ["/app/api", "healthcheck"]
ENTRYPOINT ["/app/api"]

# Production image (default, must stay last): API, migrate and bootstrap. No seed command.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
WORKDIR /app
COPY --from=build /out/api /out/migrate /out/bootstrap /app/
COPY database/migrations /app/database/migrations
ENV MIGRATIONS_DIR=/app/database/migrations
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/app/api", "healthcheck"]
ENTRYPOINT ["/app/api"]
