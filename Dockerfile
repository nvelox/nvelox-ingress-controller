# nvelox-ingress-controller — runs as a sidecar next to nvelox.
#
# Pinned base images for reproducibility; bump the alpine tag in lock
# step with security patches. Static binary, no CGO, -trimpath strips
# build paths from the binary for smaller diffable layers.

FROM golang:1.25-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" \
    -o /out/nvelox-ingress-controller .

FROM alpine:3.20
RUN addgroup -S app && adduser -S -G app app
COPY --from=builder /out/nvelox-ingress-controller /usr/local/bin/nvelox-ingress-controller
USER app:app
ENTRYPOINT ["/usr/local/bin/nvelox-ingress-controller"]
