# Build: docker build -t xffdns .
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/ganeshphutane-g/xffdns/internal/cli.Version=${VERSION}" -o /xffdns .

# No shell, no package manager, not root. Docker lets containers on its own
# networks bind ports under 1024 without root; see the README for the host
# network.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /xffdns /usr/local/bin/xffdns
WORKDIR /etc/xffdns
EXPOSE 53/udp 53/tcp 853/tcp 9354/tcp
ENTRYPOINT ["/usr/local/bin/xffdns"]
CMD ["run", "-c", "/etc/xffdns/xffdns.yaml"]
