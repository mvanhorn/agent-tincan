# Tincan relay image: builds the static relay binary and runs it with
# the state directory on a volume. Build it from the repo root:
#
#   docker build -f examples/docker/Dockerfile -t tincan-relay .
#
# and run it as docs/container.md describes.
FROM golang:1.27.4 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.Version=${VERSION}" -o /tincan ./cmd/tincan
RUN mkdir -p /state

FROM gcr.io/distroless/static-debian12:nonroot
# The relay runs as nonroot (UID 65532) and keeps everything in /state:
# pre-create it owned by nonroot, since a fresh named volume starts out
# root-owned and the relay could not write its state there. The unprivileged
# --port keeps the relay off port 80, which nonroot cannot bind.
COPY --from=build --chown=65532:65532 /state /state
COPY --from=build /tincan /tincan
VOLUME ["/state"]
ENTRYPOINT ["/tincan"]
CMD ["relay", "--state-dir", "/state", "--port", "8787"]
