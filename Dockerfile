FROM golang:1.27.1 AS build

WORKDIR /src

ARG GOPROXY
RUN test -n "$GOPROXY"
ENV GOPROXY="$GOPROXY"

COPY go.mod go.sum ./
RUN go mod download

COPY cmd cmd
COPY internal internal

ARG GIT_COMMIT
RUN test -n "$GIT_COMMIT"

ENV CGO_ENABLED=0
RUN go build -trimpath -o /out/chat-bridge ./cmd/chat-bridge

FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/chat-bridge /chat-bridge

ARG GIT_COMMIT
ENV GIT_COMMIT="$GIT_COMMIT"

EXPOSE 8080

ENTRYPOINT ["/chat-bridge", "-config", "/config.yaml"]
