FROM golang:1.26-alpine AS build
WORKDIR /src
ENV GOPROXY=https://goproxy.cn,direct
COPY go.mod go.sum ./
COPY sdk/plugin ./sdk/plugin
COPY third_party/anydoc-go ./third_party/anydoc-go
RUN go mod download
COPY examples/web-search-plugin ./examples/web-search-plugin
RUN CGO_ENABLED=0 go build -o /out/web-search-plugin ./examples/web-search-plugin

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/web-search-plugin /web-search-plugin
ENTRYPOINT ["/web-search-plugin"]
