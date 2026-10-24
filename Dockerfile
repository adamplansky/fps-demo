FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod .
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /fps-demo .
FROM alpine:3.21
RUN addgroup -S app && adduser -S -G app app
COPY --from=build /fps-demo /fps-demo
COPY web /web
RUN mkdir -p /data/demos && chown -R app:app /data /web
USER app
EXPOSE 8080
ENTRYPOINT ["/fps-demo"]
