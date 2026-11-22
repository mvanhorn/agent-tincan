# Tincan relay image: builds the static relay binary and runs it with
# the state directory on a volume. Build it from the repo root:
#
#   docker build -f examples/docker/Dockerfile -t tincan-relay .
#
# and run it as docs/container.md describes.
FROM golang:1 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.Version=${VERSION}" -o /tincan ./cmd/tincan

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /tincan /tincan
VOLUME ["/state"]
ENTRYPOINT ["/tincan"]
CMD ["relay", "--state-dir", "/state"]
