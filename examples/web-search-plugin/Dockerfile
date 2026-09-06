FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY sdk/plugin ./sdk/plugin
COPY examples/web-search-plugin ./examples/web-search-plugin
RUN CGO_ENABLED=0 go build -o /out/web-search-plugin ./examples/web-search-plugin

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/web-search-plugin /web-search-plugin
ENTRYPOINT ["/web-search-plugin"]
