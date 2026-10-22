FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/ganeshphutane-g/wgresolve/internal/cli.Version=${VERSION}" -o /wgresolve .

FROM gcr.io/distroless/static-debian12
COPY --from=build /wgresolve /wgresolve
ENTRYPOINT ["/wgresolve"]
CMD ["run"]
