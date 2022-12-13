FROM alpine:3.17.0

ARG CREATED
ARG REVISION
ARG VERSION
ARG TITLE
ARG SOURCE
ARG AUTHORS
LABEL org.opencontainers.image.created=$CREATED \
        org.opencontainers.image.revision=$REVISION \
        org.opencontainers.image.title=$TITLE \
        org.opencontainers.image.source=$SOURCE \
        org.opencontainers.image.version=$VERSION \
        org.opencontainers.image.authors=$AUTHORS \
        org.opencontainers.image.vendor="Habx"

ENV TZ=Europe/Paris
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /go/src/github.com/habx/tool-k8s-nodes-cleaner/

COPY dist/k8s-nodes-cleaner_linux_amd64/k8s-nodes-cleaner_linux_amd64 /go/src/github.com/habx/tool-k8s-nodes-cleaner/k8s-nodes-cleaner_linux_amd64

ENTRYPOINT ["./k8s-nodes-cleaner_linux_amd64"]
