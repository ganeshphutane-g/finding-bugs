# Build: docker build -t pushprobe .
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/ganeshphutane-g/pushprobe/internal/cli.Version=${VERSION}" -o /pushprobe .

# No shell, no package manager, not root. Pings use the ping sockets Docker
# allows in containers; no extra capability is needed.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /pushprobe /usr/local/bin/pushprobe
WORKDIR /etc/pushprobe
ENTRYPOINT ["/usr/local/bin/pushprobe"]
CMD ["run", "-c", "/etc/pushprobe/pushprobe.yaml"]
