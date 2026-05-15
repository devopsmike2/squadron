ARG TELEMETRYGEN_VERSION=latest

FROM golang:1.25 AS build
ARG TELEMETRYGEN_VERSION
RUN go install github.com/open-telemetry/opentelemetry-collector-contrib/cmd/telemetrygen@${TELEMETRYGEN_VERSION}

FROM gcr.io/distroless/base-debian12
COPY --from=build /go/bin/telemetrygen /telemetrygen
ENTRYPOINT ["/telemetrygen"]
