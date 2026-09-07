# syntax=docker/dockerfile:1

# ---- build: binário estático do Bifrost ----
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bifrost .

# ---- runtime: Debian + Chrome real (mesmo google-chrome-stable do host) ----
FROM debian:bookworm-slim

# Chrome do repositório do Google — não Chromium: o login do Google é mais
# confiável com o branding Chrome. Fontes para renderizar a UI do Gemini.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl gnupg wget \
        fonts-liberation fonts-noto-color-emoji \
    && install -d -m 0755 /etc/apt/keyrings \
    && wget -qO- https://dl.google.com/linux/linux_signing_key.pub | gpg --dearmor -o /etc/apt/keyrings/google.gpg \
    && echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/google.gpg] http://dl.google.com/linux/chrome/deb/ stable main" \
        > /etc/apt/sources.list.d/google-chrome.list \
    && apt-get update \
    && apt-get install -y google-chrome-stable \
    && rm -rf /var/lib/apt/lists/*

# uid 1000 = mesmo uid do usuário do host: bind mount do profile com
# permissão de escrita, sem chown.
RUN useradd --create-home --uid 1000 bifrost \
    && mkdir -p /data/chrome-profile \
    && chown -R bifrost:bifrost /data

USER bifrost
ENV HOME=/home/bifrost
WORKDIR /data
COPY --from=build /out/bifrost /usr/local/bin/bifrost

# Padrões de container: keystore basic (sem gnome-keyring dentro do
# container — o profile cifra com chave interna), sandbox off (o seccomp
# default do Docker bloqueia o sandbox do Chrome; o isolamento fica por
# conta do container), headless, porta interna fixa :8080.
ENV BIFROST_ADDR=:8080 \
    BIFROST_PROFILE=/data/chrome-profile \
    BIFROST_HEADLESS=true \
    BIFROST_PASSWORD_STORE=basic \
    BIFROST_NO_SANDBOX=true \
    BIFROST_CHROME=/usr/bin/google-chrome-stable

EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD curl -fsS http://localhost:8080/health || exit 1

ENTRYPOINT ["bifrost"]
CMD ["serve"]
