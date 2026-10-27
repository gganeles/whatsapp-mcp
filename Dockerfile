# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/whatsapp-mcp . \
    && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/whatsapp-mcp /usr/local/bin/whatsapp-mcp
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV WHATSAPP_MCP_DATA=/data \
    WHATSAPP_MCP_HTTP=0.0.0.0:8080
VOLUME /data
EXPOSE 8080
USER nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s CMD ["whatsapp-mcp", "healthcheck"]
ENTRYPOINT ["whatsapp-mcp"]
CMD ["serve"]
