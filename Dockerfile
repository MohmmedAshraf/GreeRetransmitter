FROM golang:1.23-alpine AS builder
LABEL stage=builder

RUN apk add --no-cache build-base git

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go mod vendor && go mod verify
RUN --mount=type=cache,id=gree_retransmitter,target=/root/.cache/go-build GOOS=linux GOARCH=amd64 GOMAXPROCS=4 go build -gcflags="all=-c=4" -ldflags="-w -s" -o /src/bin/gree .


FROM alpine:latest
# Create appuser.
ENV USER=appuser
ENV UID=10001
RUN adduser \
    --disabled-password \
    --gecos "" \
    --home "/nonexistent" \
    --shell "/sbin/nologin" \
    --no-create-home \
    --uid "${UID}" \
    "${USER}"

RUN apk add --no-cache opus opus-dev

WORKDIR /src
COPY --from=builder /src/bin/ ./bin

USER appuser:appuser

ENTRYPOINT ["/src/bin/gree"]
