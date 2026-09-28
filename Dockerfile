FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /lfscan ./cmd/lfscan

FROM gcr.io/distroless/static-debian12
COPY --from=build /lfscan /lfscan
ENV LFSCAN_DATA_DIR=/data LFSCAN_ADDR=:8080
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/lfscan", "serve"]
