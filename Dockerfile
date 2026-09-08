FROM golang:1-alpine

RUN apk add --no-cache \
    bash \
    ca-certificates \
    dosfstools \
    git \
    mtools \
    xorriso

WORKDIR /work

CMD ["/bin/bash"]
