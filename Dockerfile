# Build the manager binary
FROM registry.access.redhat.com/ubi9/go-toolset:1.25 AS builder

USER 0

WORKDIR /workspace
COPY . .
# Build
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -a -o manager main.go

# Extract the commit reference from which the image is built
RUN git rev-parse --short HEAD > /commit-reference.txt

FROM registry.access.redhat.com/ubi9/ubi-minimal:latest@sha256:b9b10f42d7eba7ad4a6d5ef26b7d34fdc892b2ffe59b8d0372ec884008569eb6

# Copy the commit reference from the builder
COPY --from=builder /commit-reference.txt /commit-reference.txt

WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]

ARG QUAY_TAG_EXPIRATION
LABEL "quay.expires-after"=${QUAY_TAG_EXPIRATION}
