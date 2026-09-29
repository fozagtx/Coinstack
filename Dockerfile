# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=$(git describe --tags --always 2>/dev/null || echo dev)" -o /coinstack ./cmd/coinstack

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /coinstack /coinstack
EXPOSE 8080
ENTRYPOINT ["/coinstack"]
