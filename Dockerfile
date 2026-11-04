# Build a static binary, so the final image needs no C library.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /objectcacheapi ./cmd/objectcacheapi

# Distroless has no shell or package manager, and this variant runs as a non-root user whose home directory is
# writable, which is where the snapshot file goes.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /home/nonroot
COPY --from=build /objectcacheapi /usr/local/bin/objectcacheapi
EXPOSE 8080
ENTRYPOINT ["objectcacheapi"]
