FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY sdk/plugin ./sdk/plugin
COPY examples/model-provider-plugin ./examples/model-provider-plugin
RUN CGO_ENABLED=0 go build -o /out/model-provider-plugin ./examples/model-provider-plugin

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/model-provider-plugin /model-provider-plugin
ENTRYPOINT ["/model-provider-plugin"]
