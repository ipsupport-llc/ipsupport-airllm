# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/ipsupport-airouter ./cmd/ipsupport-airouter

FROM alpine:3.20
RUN adduser -D -u 10001 app
COPY --from=build /out/ipsupport-airouter /usr/local/bin/ipsupport-airouter
USER app
EXPOSE 8080
ENTRYPOINT ["ipsupport-airouter"]
