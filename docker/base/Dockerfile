FROM alpine:3.22
RUN apk add --no-cache bash git ripgrep ca-certificates tzdata
ENV HOME=/root \
    OPENVAALA_CONFIG=/data/config.yaml \
    OPENVAALA_DATA_DIR=/data \
    OPENVAALA_SERVER_LISTEN=0.0.0.0:8790
EXPOSE 8790 8791
