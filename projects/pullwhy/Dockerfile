# Build: docker build -t pullwhy .
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/ganeshphutane-g/pullwhy/internal/cli.Version=${VERSION}" -o /pullwhy .

# No shell and no package manager. It runs as root because it talks to the
# daemon's socket, which is root's; it only reads.
FROM gcr.io/distroless/static-debian12
COPY --from=build /pullwhy /usr/local/bin/pullwhy
ENTRYPOINT ["/usr/local/bin/pullwhy"]
