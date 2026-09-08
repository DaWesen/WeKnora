FROM golang:1.26-alpine AS build
WORKDIR /src
ENV GOPROXY=https://goproxy.cn,direct
COPY go.mod go.sum ./
COPY sdk/plugin ./sdk/plugin
COPY third_party/anydoc-go ./third_party/anydoc-go
COPY examples/retriever-plugin ./examples/retriever-plugin
RUN --mount=type=cache,target=/go/pkg/mod CGO_ENABLED=0 go build -o /out/retriever-plugin ./examples/retriever-plugin

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/retriever-plugin /retriever-plugin
ENTRYPOINT ["/retriever-plugin"]
