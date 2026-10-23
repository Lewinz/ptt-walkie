# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ptt-server ./cmd/ptt-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ptt-server /ptt-server
ENV PTT_ADDR=:8080 PTT_DATA_DIR=/data
VOLUME /data
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/ptt-server"]
