ARG GO_VERSION=1.26.9
FROM golang:${GO_VERSION}-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/policy .

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /var/lib/postfix-policy/queue
EXPOSE 2555
ENTRYPOINT ["/usr/bin/policy"]
CMD ["-conf", "/etc/policy.json"]
COPY --from=builder /out/policy /usr/bin/policy
