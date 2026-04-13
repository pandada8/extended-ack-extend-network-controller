FROM golang:1.24 AS builder

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o /workspace/bin/manager .

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /

COPY --from=builder /workspace/bin/manager /manager

USER 65532:65532

ENTRYPOINT ["/manager"]
