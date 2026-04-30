# cp-stub — minimal CP stand-in for the local production-shape harness.
# See main.go for the rationale. Self-contained build, no module deps.

FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /cp-stub .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /cp-stub /cp-stub
EXPOSE 9090
ENTRYPOINT ["/cp-stub"]
