FROM golang:1.26-alpine AS build
WORKDIR /src
ENV GOPROXY=https://goproxy.cn,direct
COPY go.mod go.sum ./
COPY sdk/plugin ./sdk/plugin
COPY third_party/anydoc-go ./third_party/anydoc-go
COPY examples/model-provider-plugin ./examples/model-provider-plugin
RUN --mount=type=cache,target=/go/pkg/mod CGO_ENABLED=0 go build -o /out/model-provider-plugin ./examples/model-provider-plugin

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/model-provider-plugin /model-provider-plugin
ENTRYPOINT ["/model-provider-plugin"]
