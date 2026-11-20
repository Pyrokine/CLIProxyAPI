FROM golang:1.27.4-alpine3.24 AS builder

WORKDIR /app

RUN apk add --no-cache gcc musl-dev

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN test -s static/management.html && \
    ! grep -q 'Management panel asset is not included in this Docker image' static/management.html

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=1 GOOS=linux go build \
    -ldflags="-s -w \
      -X 'main.version=${VERSION}' \
      -X 'main.commit=${COMMIT}' \
      -X 'main.buildDate=${BUILD_DATE}' \
      -X 'github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo.UpdateProtocol=2' \
      -X \
      'github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo.DataFormats=management=1:5:5;usage=1:9:9;postgres=1:1:1'" \
    -o ./CLIProxyAPI ./cmd/server/

FROM alpine:3.24.4

RUN apk add --no-cache tzdata

RUN mkdir -p /CLIProxyAPI/static /CLIProxyAPI/config

COPY --from=builder ./app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI
COPY --from=builder ./app/static/management.html /CLIProxyAPI/static/management.html

COPY config.example.yaml /CLIProxyAPI/config.example.yaml

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai
ENV MANAGEMENT_STATIC_PATH=/CLIProxyAPI/static

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["./CLIProxyAPI", "--config", "/CLIProxyAPI/config/config.yaml"]
