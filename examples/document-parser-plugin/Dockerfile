FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY sdk/plugin ./sdk/plugin
COPY examples/document-parser-plugin ./examples/document-parser-plugin
RUN CGO_ENABLED=0 go build -o /out/document-parser-plugin ./examples/document-parser-plugin

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/document-parser-plugin /document-parser-plugin
ENTRYPOINT ["/document-parser-plugin"]
