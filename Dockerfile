# syntax = docker/dockerfile:1.22
########################################

FROM --platform=${TARGETARCH} scratch AS release

COPY --from=gcr.io/distroless/static-debian13:nonroot . .
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/custom-metrics /bin/custom-metrics

ENTRYPOINT ["/bin/custom-metrics"]
