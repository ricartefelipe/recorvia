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
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
USER 101
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
