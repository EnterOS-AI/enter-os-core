# cp-stub — minimal CP stand-in for the local production-shape harness.
# See main.go for the rationale. Self-contained build, no module deps.

FROM golang:1.25-alpine@sha256:c4ea15b4a7912716eb362a022e2b12317762eca387423760bc59c0f9ae69423c AS builder
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /cp-stub .

FROM alpine:3.20@sha256:c64c687cbea9300178b30c95835354e34c4e4febc4badfe27102879de0483b5e
RUN apk add --no-cache ca-certificates
COPY --from=builder /cp-stub /cp-stub
EXPOSE 9090
ENTRYPOINT ["/cp-stub"]
