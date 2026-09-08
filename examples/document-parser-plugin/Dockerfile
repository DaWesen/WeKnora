FROM golang:1.26-alpine AS build
WORKDIR /src
ENV GOPROXY=https://goproxy.cn,direct
COPY go.mod go.sum ./
COPY sdk/plugin ./sdk/plugin
COPY third_party/anydoc-go ./third_party/anydoc-go
COPY examples/document-parser-plugin ./examples/document-parser-plugin
RUN --mount=type=cache,target=/go/pkg/mod CGO_ENABLED=0 go build -o /out/document-parser-plugin ./examples/document-parser-plugin

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/document-parser-plugin /document-parser-plugin
ENTRYPOINT ["/document-parser-plugin"]
