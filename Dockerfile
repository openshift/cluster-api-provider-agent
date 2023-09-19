# Build the manager binary
FROM registry.ci.openshift.org/openshift/release:golang-1.20 as builder

WORKDIR /workspace
COPY . .
# Build
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -a -o manager main.go

FROM registry.ci.openshift.org/ocp/4.14:base
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]

ARG QUAY_TAG_EXPIRATION
LABEL "quay.expires-after"=${QUAY_TAG_EXPIRATION}
