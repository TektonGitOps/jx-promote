FROM gcr.io/jenkinsxio/jx-boot:3.0.768

ARG BUILD_DATE
ARG VERSION
ARG REVISION
ARG TARGETARCH
ARG TARGETOS

LABEL maintainer="jenkins-x"

# lets get the jx command to download the correct plugin version
ENV JX_PROMOTE_VERSION $VERSION

RUN jx promote --help
