# Build: docker build -t alertlines .
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/ganeshphutane-g/alertlines/internal/cli.Version=${VERSION}" -o /alertlines .

# No shell, no package manager, not root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /alertlines /usr/local/bin/alertlines
WORKDIR /etc/alertlines
EXPOSE 9097
ENTRYPOINT ["/usr/local/bin/alertlines"]
CMD ["run", "-c", "/etc/alertlines/alertlines.yaml"]
