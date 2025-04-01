FROM golang:1 AS builder

COPY . /app
WORKDIR /app

RUN make build

FROM gcr.io/distroless/static-debian12

COPY --from=builder /app/bin/go-chia-crawler /go-chia-crawler

ENTRYPOINT ["/go-chia-crawler"]
CMD ["crawl"]
