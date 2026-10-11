FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /notify-qq ./cmd/notify-qq \
    && mkdir /data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /notify-qq /notify-qq
COPY --from=build --chown=65532:65532 /data /data
USER 65532:65532
ENV LISTEN_ADDR=0.0.0.0:8080 DATA_DIR=/data
EXPOSE 8080
ENTRYPOINT ["/notify-qq"]
