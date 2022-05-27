FROM golang as builder
ENV GO111MODULE=on
WORKDIR /app
COPY . .
RUN go mod download
RUN env CGO_ENABLED=0 go build -o /policy .

FROM alpine:3.16
RUN apk add --no-cache tzdata ca-certificates
COPY --from=builder /policy /usr/bin/policy
ENTRYPOINT ["/usr/bin/policy"]
CMD ["/usr/bin/policy", "-conf", "/etc/policy.json"]
