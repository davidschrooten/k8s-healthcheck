FROM golang:1.12-alpine

RUN apk --no-cache --virtual temp add git dep

RUN apk --no-cache add redis mariadb-client

RUN mkdir -p /go/src/k8s-healthcheck

ADD main.go /go/src/k8s-healthcheck/main.go
ADD Gopkg.lock /go/src/k8s-healthcheck/Gopkg.lock
ADD Gopkg.toml /go/src/k8s-healthcheck/Gopkg.toml

WORKDIR /go/src/k8s-healthcheck

RUN dep ensure && \
    go build main.go && \
    chmod +x main && \
    mv main /usr/bin/k8s-healthcheck && \
    rm -rf /go/src/k8s-healthcheck && \
    apk del temp

EXPOSE 9000

ENTRYPOINT ["k8s-healthcheck"]