FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /hello .

FROM alpine:3.21
COPY --from=build /hello /hello
USER 65532:65532
EXPOSE 8000
ENTRYPOINT ["/hello"]
