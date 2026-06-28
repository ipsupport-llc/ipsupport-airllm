# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/ipsupport-airllm ./cmd/ipsupport-airllm

FROM alpine:3.20
RUN adduser -D -u 10001 app
COPY --from=build /out/ipsupport-airllm /usr/local/bin/ipsupport-airllm
USER app
EXPOSE 8080
ENTRYPOINT ["ipsupport-airllm"]
