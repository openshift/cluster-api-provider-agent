# Build the manager binary
FROM registry.access.redhat.com/ubi9/go-toolset:1.21 as builder

USER 0

WORKDIR /workspace
COPY . .
# Build
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -a -o manager main.go

FROM quay-proxy.ci.openshift.org/openshift/ci:ocp_4.16_base-rhel9

WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]

ARG QUAY_TAG_EXPIRATION
LABEL "quay.expires-after"=${QUAY_TAG_EXPIRATION}
