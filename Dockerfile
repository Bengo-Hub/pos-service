# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS builder
WORKDIR /src
# Copy shared auth-client first (needed for replace directive)
# Build context should be from workspace root: docker build -f pos-service/Dockerfile -t pos-service:local .
COPY shared/auth-client /shared/auth-client
COPY pos-service/go.mod pos-service/go.sum ./
RUN go mod download
COPY pos-service .

RUN CGO_ENABLED=0 go build -o /out/pos ./cmd/api

FROM alpine:3.20
RUN addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=builder /out/pos /app/service
# TLS certificates directory (optional, can be mounted as volume)
RUN mkdir -p ./config/certs
USER app
EXPOSE 4000
ENV PORT=4000
ENTRYPOINT ["/app/service"]

