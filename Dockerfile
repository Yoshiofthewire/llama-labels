FROM golang:1.26.4 AS backend-builder
WORKDIR /app
COPY backend/go.mod backend/go.sum* ./backend/
RUN cd backend && go mod download
COPY backend ./backend
RUN cd backend && go build -o /app/bin/kypost-server ./cmd/main.go

FROM node:26.3.0-slim AS frontend-builder
WORKDIR /frontend
COPY frontend/package.json frontend/package-lock.json* ./
RUN npm install
COPY frontend .
RUN npm run build

FROM node:26.3.0-slim
RUN apt-get update \
	&& apt-get install -y --no-install-recommends supervisor tzdata curl ca-certificates zstd \
	&& rm -rf /var/lib/apt/lists/* \
	&& useradd -m -s /bin/bash kypost

WORKDIR /opt/kypost
COPY --from=backend-builder /app/bin/kypost-server /usr/local/bin/kypost-server
COPY --from=frontend-builder /frontend/dist /opt/kypost/frontend
COPY TUNING.md /opt/kypost/TUNING.md
COPY supervisord.conf /etc/supervisord.conf
COPY scripts /opt/kypost/scripts

RUN chmod +x /opt/kypost/scripts/*.sh

RUN curl -fsSL https://ollama.com/install.sh | sh

ENV CONFIG_DIR=/kypost/config
ENV SECRET_DIR=/kypost/private
ENV LOG_DIR=/kypost/logs
ENV STATE_DIR=/kypost/state
ENV WEB_PORT=5866
ENV TZ=America/New_York
ENV OLLAMA_BASE_URL=http://127.0.0.1:11434
ENV OLLAMA_MODEL=nemotron-3-nano:4b
ENV OLLAMA_MODELS=/kypost/ollama-models
ENV PAIRING_SECRET=

RUN mkdir -p /kypost/config /kypost/private /kypost/logs /kypost/state \
	&& mkdir -p /kypost/ollama-models \
	&& chown -R kypost:kypost /kypost /opt/kypost

VOLUME ["/kypost/config", "/kypost/private", "/kypost/logs", "/kypost/state"]
EXPOSE 5866

CMD ["/opt/kypost/scripts/entrypoint.sh"]
