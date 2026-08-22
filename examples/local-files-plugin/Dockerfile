FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/plugin/proto ./internal/plugin/proto
COPY examples/local-files-plugin ./examples/local-files-plugin
RUN CGO_ENABLED=0 go build -o /out/local-files-plugin ./examples/local-files-plugin

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/local-files-plugin /local-files-plugin
ENTRYPOINT ["/local-files-plugin"]
