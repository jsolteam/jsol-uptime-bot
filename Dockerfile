FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/uptime-bot ./cmd/uptime-bot && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/uptime-agent ./cmd/uptime-agent
RUN printf '#!/bin/sh\nwget -q -O /dev/null http://127.0.0.1:8080/health || wget --no-check-certificate -q -O /dev/null https://127.0.0.1:8080/health\n' > /out/healthcheck && chmod +x /out/healthcheck
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && addgroup -S -g 10001 app && adduser -S -D -H -u 10001 -G app app && mkdir -p /data && chown app:app /data
COPY --from=build /out/uptime-bot /app/uptime-bot
COPY --from=build /out/uptime-agent /app/uptime-agent
COPY --from=build /out/healthcheck /app/healthcheck
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/app/uptime-bot"]
