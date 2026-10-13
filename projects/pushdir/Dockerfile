# pushdir with rclone in one image.
#   docker run -d -v /srv/photos:/data:ro -v ~/.config/rclone:/config/rclone \
#     pushdir /data b2:my-photos
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /pushdir .

FROM rclone/rclone:1.75
COPY --from=build /pushdir /usr/local/bin/pushdir
ENV RCLONE_CONFIG=/config/rclone/rclone.conf
ENTRYPOINT ["/usr/local/bin/pushdir"]
