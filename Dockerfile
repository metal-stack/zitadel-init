FROM alpine:3.22
LABEL maintainer="metal-stack authors <info@metal-stack.io>"
COPY bin/zitadel-init-linux-amd64 /zitadel-init
ENTRYPOINT ["/zitadel-init"]
