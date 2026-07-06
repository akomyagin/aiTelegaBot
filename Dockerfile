# Multi-stage build for the aiTelegaBot service binary.
#
# The whole project is pure Go (SQLite via modernc.org/sqlite, no CGO), so we
# build a static binary and run it from a minimal distroless image.

# --- builder ---
FROM golang:1.22 AS builder
WORKDIR /src

# Cache modules first.
COPY go.mod ./
# COPY go.sum ./            # uncomment once dependencies are added
RUN go mod download || true

COPY . .

# CGO off => static binary, tiny runtime image, no C toolchain needed.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /bot ./cmd/bot

# --- runtime ---
FROM gcr.io/distroless/static:nonroot
WORKDIR /app

# State (SQLite, MTProto session in Фаза 2) lives on a mounted volume at /data.
COPY --from=builder /bot /app/bot

USER nonroot:nonroot
ENTRYPOINT ["/app/bot"]
