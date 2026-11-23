# Proxy reverso de produção (ADR-086): Caddy com HTTPS automático e a
# configuração embutida na imagem (ADR-020: sem bind mounts).
# Uso (contexto é a pasta deploy/producao/caddy):
#   docker build -t recorvia-proxy:<versão> deploy/producao/caddy
FROM caddy:2.11.9-alpine
COPY Caddyfile /etc/caddy/Caddyfile
# Recusa no build uma configuração inválida (com valores de exemplo para as
# variáveis lidas na partida).
RUN RECORVIA_DOMAIN=app.example.com KEYCLOAK_DOMAIN=auth.example.com ACME_EMAIL=ops@example.com \
    KEYCLOAK_ADMIN_ALLOWED_CIDRS="0.0.0.0/0 ::/0" \
    caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
