FROM golang:1.27.1 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN go test ./... && go vet ./...
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /unfoldmail-connect .
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /unfoldmail-connect /unfoldmail-connect
LABEL org.opencontainers.image.title="UnfoldMail Connect" \
      org.opencontainers.image.source="https://github.com/Web-Network-Solution/UnfoldMail-Connect" \
      org.opencontainers.image.vendor="Web Network Solutions" \
      org.opencontainers.image.licenses="MIT"
USER 65532:65532
EXPOSE 1993 1465 1995 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/unfoldmail-connect", "healthcheck"]
ENTRYPOINT ["/unfoldmail-connect"]
