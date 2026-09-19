FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /notes .

FROM alpine:3.21 AS server
COPY --from=build /notes /notes
USER 65532:65532
EXPOSE 8000 8001
CMD ["/notes"]
