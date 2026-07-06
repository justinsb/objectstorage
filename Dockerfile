FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /objectstorage ./cmd/objectstorage
RUN mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /objectstorage /objectstorage
# 65532 is distroless's nonroot user; pre-owning /data makes the default
# volume writable. For bind mounts, either chown the host dir to 65532 or
# override the container user.
COPY --from=build --chown=65532:65532 /data /data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/objectstorage", "--data-dir=/data", "--listen=:8080"]
