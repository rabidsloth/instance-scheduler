FROM golang:1.19-alpine AS builder

WORKDIR /go/app
COPY . /go/app
RUN go build -o instance-scheduler main.go

FROM alpine

RUN apk update && apk upgrade && apk add ca-certificates tzdata && rm -rf /var/cache/apk/*
ENV TZ=America/Los_Angeles
RUN mkdir /instance-scheduler
WORKDIR /instance-scheduler
COPY --from=builder /go/app/instance-scheduler /instance-scheduler/instance-scheduler
COPY --from=builder /go/app/config.yaml /instance-scheduler/config.yaml
CMD ["/instance-scheduler/instance-scheduler"]