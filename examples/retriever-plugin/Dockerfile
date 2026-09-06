FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY sdk/plugin ./sdk/plugin
COPY examples/retriever-plugin ./examples/retriever-plugin
RUN CGO_ENABLED=0 go build -o /out/retriever-plugin ./examples/retriever-plugin

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/retriever-plugin /retriever-plugin
ENTRYPOINT ["/retriever-plugin"]
