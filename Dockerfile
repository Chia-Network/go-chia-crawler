FROM golang:1 as builder

COPY . /app
WORKDIR /app

RUN make build

FROM golang:1

COPY --from=builder /app/bin/go-chia-crawler /go-chia-crawler

ENTRYPOINT ["/go-chia-crawler"]
CMD ["crawl"]
