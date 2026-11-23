# Interface web do Recorvia: build com Node e entrega por nginx sem root.
# Uso (contexto é a pasta web/): docker build -t recorvia-web:local web
FROM node:22.23.5-alpine3.24 AS build
WORKDIR /app
ENV CI=true
COPY package.json package-lock.json .npmrc ./
RUN npm ci
COPY . .
RUN npm run build

# Imagem final: nginx sem privilégios (usuário 101), escutando na porta 8080.
FROM nginxinc/nginx-unprivileged:1.31.7-alpine3.24
# A imagem base pode não incorporar correções de segurança já publicadas no
# branch estável do Alpine 3.24 (verificadas pelo Trivy no job image-scan,
# ADR-057): os pacotes do sistema são atualizados no build, sem trocar de
# versão do Alpine.
USER root
RUN apk upgrade --no-cache
COPY nginx.conf /etc/nginx/conf.d/default.conf
# Gera, na partida, a lista de proxies confiáveis para o X-Forwarded-For.
COPY --chmod=755 real-ip.sh /docker-entrypoint.d/40-recorvia-real-ip.sh
COPY --from=build /app/dist /usr/share/nginx/html
USER 101
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
