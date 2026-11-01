# Two images from one file: `--target api` (the Go binary on distroless) and `--target web` (the Angular app on nginx).

FROM golang:1.27-alpine AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

FROM gcr.io/distroless/static-debian12:nonroot AS api
COPY --from=go-build /out/api /app/api
ENV LISTEN_ADDR=:8080
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/app/api"]

FROM node:22-bookworm-slim AS web-build
ENV NX_DAEMON=false
WORKDIR /src
COPY package*.json ./
RUN npm ci
COPY nx.json eslint.config.mjs .postcssrc.json ./
COPY apps/ apps/
RUN npx nx build web

FROM nginxinc/nginx-unprivileged:stable-alpine AS web
COPY docker/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=web-build /src/dist/apps/web/browser /usr/share/nginx/html
EXPOSE 8080
USER 101
